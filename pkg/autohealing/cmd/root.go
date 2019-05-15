/*
Copyright 2019 The Kubernetes Authors.

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

package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/mitchellh/go-homedir"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"k8s.io/cloud-provider-openstack/pkg/autohealing/config"
	"k8s.io/cloud-provider-openstack/pkg/autohealing/controller"
)

var (
	cfgFile string
	isDebug bool
	conf    config.Config
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "k8s-auto-healer",
	Short: "Auto healer for Kubernetes cluster.",
	Long: "Auto healer is responsible for monitoring the nodes’ status periodically in the cloud environment, searching " +
		"for unhealthy instances and triggering replacements when needed, maximizing the cluster’s efficiency and performance. " +
		"OpenStack is supported by default.",

	Run: func(cmd *cobra.Command, args []string) {
		autohealer := controller.NewController(conf)
		autohealer.Start()

		sigCh := make(chan os.Signal)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
	},
}

// Execute adds all child commands to the root command sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(-1)
	}
}

func init() {
	log.SetOutput(os.Stdout)

	cobra.OnInitialize(initConfig)

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.kube_autohealer_config.yaml)")
	rootCmd.PersistentFlags().BoolVar(&isDebug, "debug", false, "Print more detailed information.")
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {
	if cfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(cfgFile)
	} else {
		// Find home directory.
		home, err := homedir.Dir()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		// Search config in home directory with name ".kube_autohealer_config" (without extension).
		viper.AddConfigPath(home)
		viper.SetConfigName(".kube_autohealer_config")
	}

	viper.AutomaticEnv() // read in environment variables that match

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err != nil {
		log.WithFields(log.Fields{"error": err}).Fatal("Failed to read config file")
	}

	log.WithFields(log.Fields{"file": viper.ConfigFileUsed()}).Info("Using config file")

	conf = config.NewConfig()
	if err := viper.Unmarshal(&conf); err != nil {
		log.WithFields(log.Fields{"error": err}).Fatal("Unable to decode the configuration")
	}
	if conf.ClusterName == "" {
		log.Fatal("cluster-name is required in the configuration.")
	}

	if isDebug {
		log.SetLevel(log.DebugLevel)
	}
}
