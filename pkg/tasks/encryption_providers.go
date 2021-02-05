/*
Copyright 2019 The KubeOne Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tasks

import (
	"context"
	"errors"
	"fmt"
	"time"

	kubeoneapi "k8c.io/kubeone/pkg/apis/kubeone"
	"k8c.io/kubeone/pkg/scripts"
	"k8c.io/kubeone/pkg/ssh"
	"k8c.io/kubeone/pkg/state"
	"k8c.io/kubeone/pkg/templates"

	encryptionproviders "k8c.io/kubeone/pkg/templates/encryption-providers"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	apiserverconfigv1 "k8s.io/apiserver/pkg/apis/config/v1"
	dynclient "sigs.k8s.io/controller-runtime/pkg/client"
	kyaml "sigs.k8s.io/yaml"
)

// download the configuration from leader
func FetchEncryptionProvidersFile(s *state.State) error {
	s.Logger.Infof("Downloading EncryptionProviders configuration file...")
	host, err := s.Cluster.Leader()
	if err != nil {
		return err
	}

	conn, err := s.Connector.Connect(host)
	if err != nil {
		return err
	}
	fileName := s.GetEncryptionProviderConfigName()
	config, _, _, err := conn.Exec(fmt.Sprintf("sudo cat /etc/kubernetes/encryption-providers/%s", fileName))
	if err != nil {
		return err
	}

	s.LiveCluster.EncryptionConfiguration.Config = &apiserverconfigv1.EncryptionConfiguration{}
	err = kyaml.UnmarshalStrict([]byte(config), s.LiveCluster.EncryptionConfiguration.Config)
	return err
}

func UploadIdentityFirstEncryptionConficguration(s *state.State) error {
	s.Logger.Infof("Uploading EncryptionProviders configuration file...")

	if s.LiveCluster.EncryptionConfiguration == nil ||
		s.LiveCluster.EncryptionConfiguration.Config == nil {
		return errors.New("failed to read live cluster encryption providers configuration")
	}

	oldConfig := s.LiveCluster.EncryptionConfiguration.Config.DeepCopy()

	encryptionproviders.UpdateEncryptionConfigDecryptOnly(oldConfig)

	config, err := templates.KubernetesToYAML([]runtime.Object{oldConfig})
	if err != nil {
		return err
	}
	s.Configuration.AddFile("cfg/encryption-providers.yaml", config)
	return s.RunTaskOnControlPlane(pushEncryptionConfigurationOnNode, state.RunParallel)
}

func UploadEncryptionConfigurationWithNewKey(s *state.State) error {
	s.Logger.Infof("Uploading EncryptionProviders configuration file...")

	if s.LiveCluster.EncryptionConfiguration == nil ||
		s.LiveCluster.EncryptionConfiguration.Config == nil {
		return errors.New("failed to read live cluster encryption providers configuration")
	}

	// oldConfig := s.LiveCluster.EncryptionConfiguration.Config.DeepCopy()
	if err := encryptionproviders.UpdateEncryptionConfigWithNewKey(s.LiveCluster.EncryptionConfiguration.Config); err != nil {
		return err
	}

	config, err := templates.KubernetesToYAML([]runtime.Object{s.LiveCluster.EncryptionConfiguration.Config})
	if err != nil {
		return err
	}

	s.Configuration.AddFile("cfg/encryption-providers.yaml", config)
	return s.RunTaskOnControlPlane(pushEncryptionConfigurationOnNode, state.RunParallel)
}

func UploadEncryptionConfigurationWithoutOldKey(s *state.State) error {
	s.Logger.Infof("Uploading EncryptionProviders configuration file...")

	if s.LiveCluster.EncryptionConfiguration == nil ||
		s.LiveCluster.EncryptionConfiguration.Config == nil {
		return errors.New("failed to read live cluster encryption providers configuration")
	}

	// oldConfig := s.LiveCluster.EncryptionConfiguration.Config.DeepCopy()

	encryptionproviders.UpdateEncryptionConfigRemoveOldKey(s.LiveCluster.EncryptionConfiguration.Config)

	config, err := templates.KubernetesToYAML([]runtime.Object{s.LiveCluster.EncryptionConfiguration.Config})
	if err != nil {
		return err
	}
	s.Configuration.AddFile("cfg/encryption-providers.yaml", config)
	return s.RunTaskOnControlPlane(pushEncryptionConfigurationOnNode, state.RunParallel)
}

func pushEncryptionConfigurationOnNode(s *state.State, node *kubeoneapi.HostConfig, conn ssh.Connection) error {
	err := s.Configuration.UploadTo(conn, s.WorkDir)
	if err != nil {
		return err
	}
	cmd, err := scripts.SaveEncryptionProvidersConfig(s.WorkDir, s.GetEncryptionProviderConfigName())
	if err != nil {
		return err
	}

	_, _, err = s.Runner.RunRaw(cmd)
	return err
}

func RewriteClusterSecrets(s *state.State) error {
	s.Logger.Infof("Rewriting cluster secrets...")
	secrets := corev1.SecretList{}
	err := s.DynamicClient.List(context.Background(), &secrets, &dynclient.ListOptions{})
	if err != nil {
		return err
	}
	for i := range secrets.Items {
		secret := secrets.Items[i]
		if err = s.DynamicClient.Update(context.Background(), &secret, &dynclient.UpdateOptions{}); err != nil {
			if kerrors.IsConflict(err) {
				err = s.DynamicClient.Get(context.Background(), types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, &secret)
				if err != nil {
					return err
				}
				if err = s.DynamicClient.Update(context.Background(), &secret, &dynclient.UpdateOptions{}); err != nil {
					return err
				}
			}
			return err
		}
	}
	return nil
}

// FIXME: Static pods are not managed by the API, so we can't simply delete them to restart.
// We should use a cleaner method to do this.
func RestartKubeAPI(s *state.State) error {
	s.Logger.Infof("Restarting KubeAPI...")
	return s.RunTaskOnControlPlane(func(s *state.State, _ *kubeoneapi.HostConfig, _ ssh.Connection) error {
		_, _, err := s.Runner.RunRaw(`docker restart $(docker ps --filter="label=io.kubernetes.container.name=kube-apiserver" -q)`)
		return err
	}, state.RunParallel)
}

func WaitForAPI(s *state.State) error {
	s.Logger.Infof("Waiting %v to ensure all components are up...", 2*timeoutNodeUpgrade)
	time.Sleep(2 * timeoutNodeUpgrade)
	return nil
}

func RemoveEncryptionProviderFile(s *state.State) error {
	s.Logger.Infof("Removing EncryptionProviders configuration file...")
	return s.RunTaskOnControlPlane(func(s *state.State, _ *kubeoneapi.HostConfig, _ ssh.Connection) error {
		cmd, err := scripts.DeleteEncryptionProvidersConfig(s.GetEncryptionProviderConfigName())
		if err != nil {
			return err
		}
		_, _, err = s.Runner.RunRaw(cmd)
		return err
	}, state.RunParallel)
}
