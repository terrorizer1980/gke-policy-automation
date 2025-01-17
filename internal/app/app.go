// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	cfg "github.com/google/gke-policy-automation/internal/config"
	"github.com/google/gke-policy-automation/internal/gke"
	"github.com/google/gke-policy-automation/internal/log"
	"github.com/google/gke-policy-automation/internal/outputs"
	pbc "github.com/google/gke-policy-automation/internal/outputs/pubsub"
	"github.com/google/gke-policy-automation/internal/outputs/storage"
	"github.com/google/gke-policy-automation/internal/policy"
	"github.com/google/gke-policy-automation/internal/version"
	"golang.org/x/exp/maps"
)

const (
	regoPackageBaseBestPractices = "gke.policy"
	regoPackageBaseScalability   = "gke.scalability"
)

var errNoPolicies = errors.New("no policies to check against")

type PolicyAutomation interface {
	LoadCliConfig(cliConfig *CliConfig, validateFn cfg.ValidateConfig) error
	Close() error
	Check() error
	CheckBestPractices() error
	CheckScalability() error
	ClusterJSONData() error
	Version() error
	PolicyCheck() error
	PolicyGenerateDocumentation() error
	ConfigureSCC(orgNumber string) error
}

type evaluationResults struct {
	m map[string]*policy.PolicyEvaluationResult
}

func (r *evaluationResults) Add(result *policy.PolicyEvaluationResult) *evaluationResults {
	if r.m == nil {
		r.m = make(map[string]*policy.PolicyEvaluationResult)
	}
	currentResult, ok := r.m[result.ClusterID]
	if !ok {
		r.m[result.ClusterID] = result
		return r
	}
	currentResult.Policies = append(currentResult.Policies, result.Policies...)
	return r
}

func (r *evaluationResults) List() []*policy.PolicyEvaluationResult {
	return maps.Values(r.m)
}

type PolicyAutomationApp struct {
	ctx                   context.Context
	config                *cfg.Config
	out                   *outputs.Output
	collectors            []outputs.ValidationResultCollector
	clusterDumpCollectors []outputs.ClusterDumpCollector
	gke                   gke.GKEClient
	discovery             gke.DiscoveryClient
	policyDocsFile        string
}

func NewPolicyAutomationApp() PolicyAutomation {
	out := outputs.NewSilentOutput()
	return &PolicyAutomationApp{
		ctx:        context.Background(),
		config:     &cfg.Config{},
		out:        out,
		collectors: []outputs.ValidationResultCollector{outputs.NewConsoleResultCollector(out)},
	}
}

func (p *PolicyAutomationApp) LoadCliConfig(cliConfig *CliConfig, validateFn cfg.ValidateConfig) error {
	var config *cfg.Config
	var err error
	if cliConfig.ConfigFile != "" {
		if config, err = newConfigFromFile(cliConfig.ConfigFile); err != nil {
			return err
		}
	} else {
		config = newConfigFromCli(cliConfig)
	}
	cfg.SetConfigDefaults(config)
	if validateFn != nil {
		if err := validateFn(*config); err != nil {
			return err
		}
	}
	return p.LoadConfig(config)
}

func (p *PolicyAutomationApp) LoadConfig(config *cfg.Config) (err error) {
	p.config = config
	if !p.config.SilentMode {
		p.out = outputs.NewStdOutOutput()
		p.collectors = []outputs.ValidationResultCollector{outputs.NewConsoleResultCollector(p.out)}
		p.clusterDumpCollectors = append(p.clusterDumpCollectors, outputs.NewOutputClusterDumpCollector(p.out))
	}
	if p.config.DumpFile != "" {
		p.gke = gke.NewGKELocalClient(p.ctx, p.config.DumpFile)
	} else {
		builder := gke.NewGKEApiClientBuilder(p.ctx)
		if p.config.CredentialsFile != "" {
			builder = builder.WithCredentialsFile(p.config.CredentialsFile)
		}
		if p.config.K8SApiConfig.Enabled {
			builder = builder.WithK8SClient(config.K8SApiConfig.ApiVersions, config.K8SApiConfig.MaxQPS)
		}

		if len(p.config.Metrics) > 0 {
			var metricQueries []gke.MetricQuery

			for _, m := range p.config.Metrics {
				metricQueries = append(metricQueries, gke.MetricQuery{Name: m.MetricName, Query: m.Query})
			}

			builder = builder.WithMetricsClient(metricQueries)
		}

		p.gke, err = builder.Build()
		if err != nil {
			return
		}
	}

	for _, o := range p.config.Outputs {
		if o.FileName != "" {
			p.collectors = append(p.collectors, outputs.NewJSONResultToFileCollector(o.FileName))
			p.clusterDumpCollectors = append(p.clusterDumpCollectors, outputs.NewFileClusterDumpCollector(o.FileName))
			p.policyDocsFile = o.FileName
		}
		if o.CloudStorage.Bucket != "" && o.CloudStorage.Path != "" {

			var storageClient *storage.CloudStorageClient
			if p.config.CredentialsFile != "" {
				storageClient, err = storage.NewCloudStorageClientWithCredentialsFile(p.ctx, p.config.CredentialsFile)
			} else {
				storageClient, err = storage.NewCloudStorageClient(p.ctx)
			}

			var storagePath = o.CloudStorage.Path

			if !o.CloudStorage.SkipDatePrefix {
				storagePath = addDatetimePrefix(storagePath, time.Now())
			}

			storageCollector, err := outputs.NewCloudStorageResultCollector(storageClient, o.CloudStorage.Bucket, storagePath)
			if err != nil {
				return err
			}
			p.collectors = append(p.collectors, storageCollector)
		}
		if len(o.PubSub.Topic) > 0 {
			var client outputs.PubSubClient
			if p.config.CredentialsFile != "" {
				client, err = pbc.NewPubSubClientWithCredentialsFile(p.ctx, o.PubSub.Project, p.config.CredentialsFile)
			} else {
				client, err = pbc.NewPubSubClient(p.ctx, o.PubSub.Project)
			}
			if err != nil {
				return err
			}
			p.collectors = append(p.collectors, outputs.NewPubSubResultCollector(client, o.PubSub.Project, o.PubSub.Topic))
		}
		if err := p.configureSccOutput(o.SecurityCommandCenter, p.config.CredentialsFile); err != nil {
			return err
		}
	}
	return
}

func (p *PolicyAutomationApp) Close() error {
	errors := make([]error, 0)
	if p.gke != nil {
		if err := p.gke.Close(); err != nil {
			log.Warnf("error when closing GKE client: %s", err)
			errors = append(errors, err)
		}
	}
	if p.discovery != nil {
		if err := p.discovery.Close(); err != nil {
			log.Warnf("error when closing discovery client: %s", err)
			errors = append(errors, err)
		}
	}
	if len(errors) > 0 {
		return errors[0]
	}
	return nil
}

func (p *PolicyAutomationApp) Check() error {
	return p.evaluateClusters([]string{regoPackageBaseBestPractices})
}

func (p *PolicyAutomationApp) CheckBestPractices() error {
	return p.evaluateClusters([]string{regoPackageBaseBestPractices})
}

func (p *PolicyAutomationApp) CheckScalability() error {
	return p.evaluateClusters([]string{regoPackageBaseScalability})
}

func (p *PolicyAutomationApp) ClusterJSONData() error {
	clusterIds, err := p.getClusters()
	if err != nil {
		p.out.ErrorPrint("could not get clusters", err)
		log.Errorf("could not get clusters: %s", err)
	}
	for _, clusterId := range clusterIds {
		cluster, err := p.gke.GetCluster(clusterId)

		if err != nil {
			p.out.ErrorPrint("could not fetch the cluster details", err)
			log.Errorf("could not fetch cluster details: %s", err)
			return err
		}
		val, err := json.MarshalIndent(cluster, "", "    ")
		if err != nil {
			log.Debugf("could not format cluster details: %s", err)
		}
		log.Debugf("cluster: " + string(val))

		for _, dumpCollector := range p.clusterDumpCollectors {
			log.Debugf("registering cluster data with cluster dump collector %s", reflect.TypeOf(dumpCollector).String())
			dumpCollector.RegisterCluster(cluster)
		}
	}
	for _, dumpCollector := range p.clusterDumpCollectors {
		colType := reflect.TypeOf(dumpCollector).String()
		log.Debugf("closing cluster dump collector %s", colType)
		p.out.ColorPrintf("%s [light_gray][bold]Writing evaluation results ...\n", outputs.ICON_INFO)
		if err := dumpCollector.Close(); err != nil {
			log.Errorf("failed to close cluster dump collector %s due to %s", colType, err)
			return err
		}
	}
	return nil
}

func (p *PolicyAutomationApp) Version() error {
	p.out.Printf("%s\n", version.Version)
	return nil
}

func (p *PolicyAutomationApp) PolicyCheck() error {
	files, err := p.loadPolicyFiles()
	if err != nil {
		p.out.ErrorPrint("loading policy files failed: ", err)
		log.Errorf("loading policy files failed: %s", err)
		return err
	}
	pa := policy.NewPolicyAgent(p.ctx)
	if err := pa.WithFiles(files, p.config.PolicyExclusions); err != nil {
		p.out.ErrorPrint("could not parse policy files", err)
		log.Errorf("could not parse policy files: %s", err)
		return err
	}
	p.out.ColorPrintf("%s [bold][green] All policies validated correctly\n", outputs.ICON_INFO)
	log.Info("All policies validated correctly")
	return nil
}

func (p *PolicyAutomationApp) PolicyGenerateDocumentation() error {
	w, err := os.OpenFile(p.policyDocsFile, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		p.out.ErrorPrint("could not open output file for writing: ", err)
		log.Errorf("could not open output file for writing: %s", err)
		return err
	}
	defer w.Close()

	files, err := p.loadPolicyFiles()
	if err != nil {
		p.out.ErrorPrint("loading policy files failed: ", err)
		log.Errorf("loading policy files failed: %s", err)
		return err
	}

	pa := policy.NewPolicyAgent(p.ctx)
	if err := pa.WithFiles(files, p.config.PolicyExclusions); err != nil {
		p.out.ErrorPrint("could not parse policy files", err)
		log.Errorf("could not parse policy files: %s", err)
		return err
	}

	documentationGenerator := outputs.NewMarkdownPolicyDocumentation(pa.GetPolicies())
	p.out.ColorPrintf("%s [light_gray][bold]Writing policy documentation ... [%s]\n", outputs.ICON_INFO, p.policyDocsFile)
	log.Infof("Writing policy documentation to file %s", p.policyDocsFile)
	if _, err := w.Write([]byte(documentationGenerator.GenerateDocumentation())); err != nil {
		p.out.ErrorPrint("could not write documentation file", err)
		log.Errorf("could not write documentation file: %s", err)
		return err
	}
	return nil
}

func (p *PolicyAutomationApp) loadPolicyFiles() ([]*policy.PolicyFile, error) {
	policyFiles := make([]*policy.PolicyFile, 0)
	for _, policyConfig := range p.config.Policies {
		var policySrc policy.PolicySource
		if policyConfig.LocalDirectory != "" {
			policySrc = policy.NewLocalPolicySource(policyConfig.LocalDirectory)
		}
		if policyConfig.GitRepository != "" {
			policySrc = policy.NewGitPolicySource(policyConfig.GitRepository,
				policyConfig.GitBranch,
				policyConfig.GitDirectory)
		}
		p.out.ColorPrintf("%s [light_gray][bold]Reading policy files... [%s]\n", outputs.ICON_INFO, policySrc)
		log.Infof("Reading policy files from %s", policySrc)
		files, err := policySrc.GetPolicyFiles()
		if err != nil {
			p.out.ErrorPrint("could not read policy files", err)
			log.Errorf("could not read policy files: %s", err)
			return nil, err
		}
		policyFiles = append(policyFiles, files...)
	}
	return policyFiles, nil
}

func (p *PolicyAutomationApp) configureSccOutput(config cfg.SecurityCommandCenterOutput, credsFile string) error {
	if config.OrganizationNumber == "" {
		return nil
	}
	log.Infof("Loading Security Command Center output")
	collector, err := outputs.NewSccCollector(p.ctx, config.OrganizationNumber, config.ProvisionSource, credsFile)
	if err != nil {
		return err
	}
	p.collectors = append(p.collectors, collector)
	return nil
}

func addDatetimePrefix(value string, time time.Time) string {
	return fmt.Sprintf("%s_%s", time.Format("20060102_1504"), value)
}

func newConfigFromFile(path string) (*cfg.Config, error) {
	return cfg.ReadConfig(path, os.ReadFile)
}

func newConfigFromCli(cliConfig *CliConfig) *cfg.Config {
	config := &cfg.Config{}
	config.SilentMode = cliConfig.SilentMode
	config.K8SApiConfig.Enabled = cliConfig.K8SCheck
	config.CredentialsFile = cliConfig.CredentialsFile
	config.DumpFile = cliConfig.DumpFile
	if cliConfig.DiscoveryEnabled {
		config.ClusterDiscovery.Enabled = true
		if cliConfig.ProjectName != "" {
			config.ClusterDiscovery.Projects = []string{cliConfig.ProjectName}
		}
	} else {
		if cliConfig.ClusterName != "" || cliConfig.ClusterLocation != "" || cliConfig.ProjectName != "" {
			config.Clusters = []cfg.ConfigCluster{
				{
					Name:     cliConfig.ClusterName,
					Location: cliConfig.ClusterLocation,
					Project:  cliConfig.ProjectName,
				},
			}
		}
	}
	if cliConfig.OutputFile != "" {
		config.Outputs = append(config.Outputs, cfg.ConfigOutput{
			FileName: cliConfig.OutputFile,
		})
	}
	if cliConfig.LocalDirectory != "" || cliConfig.GitRepository != "" {
		config.Policies = append(config.Policies, cfg.ConfigPolicy{
			LocalDirectory: cliConfig.LocalDirectory,
			GitRepository:  cliConfig.GitRepository,
			GitBranch:      cliConfig.GitBranch,
			GitDirectory:   cliConfig.GitDirectory,
		})
	}
	return config
}

func getClusterName(c cfg.ConfigCluster) (string, error) {
	if c.ID != "" {
		return c.ID, nil
	}
	if c.Name != "" && c.Location != "" && c.Project != "" {
		return gke.GetClusterName(c.Project, c.Location, c.Name), nil
	}
	return "", fmt.Errorf("cluster mandatory parameters not set (project, name, location)")
}
