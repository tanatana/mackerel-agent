package spec

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Songmu/retry"
	"github.com/mackerelio/golib/logging"
	"github.com/mackerelio/mackerel-client-go"

	"github.com/mackerelio/mackerel-agent/config"
)

// This Generator collects metadata about cloud instances.
// Currently EC2, AzureVM and GCE are supported.
// EC2: http://docs.aws.amazon.com/AWSEC2/latest/UserGuide/AESDG-chapter-instancedata.html
// GCE: https://developers.google.com/compute/docs/metadata
// AzureVM: https://docs.microsoft.com/azure/virtual-machines/virtual-machines-instancemetadataservice-overview

// CloudGenerator definition
type CloudGenerator struct {
	CloudMetaGenerator
}

// Generate generates metadata
func (c *CloudGenerator) Generate() (interface{}, error) {
	return c.CloudMetaGenerator.Generate()
}

// CloudMetaGenerator interface of metadata generator for each cloud platform
type CloudMetaGenerator interface {
	Generate() (*mackerel.Cloud, error)
	SuggestCustomIdentifier() (string, error)
}

type ec2Generator interface {
	CloudMetaGenerator
	IsEC2(ctx context.Context) bool
}

type gceGenerator interface {
	CloudMetaGenerator
	IsGCE(ctx context.Context) bool
}

type azureVMGenerator interface {
	CloudMetaGenerator
	IsAzureVM(ctx context.Context) bool
}

var cloudLogger = logging.GetLogger("spec.cloud")

var ec2BaseURL, gceMetaURL, azureVMBaseURL *url.URL

type cloudGeneratorSuggester struct {
	ec2Generator     ec2Generator
	gceGenerator     gceGenerator
	azureVMGenerator azureVMGenerator
}

// Suggest returns suitable CloudGenerator
func (s *cloudGeneratorSuggester) Suggest(conf *config.Config) *CloudGenerator {
	// if CloudPlatform is specified, return corresponding one
	switch conf.CloudPlatform {
	case config.CloudPlatformNone:
		return nil
	case config.CloudPlatformEC2:
		return &CloudGenerator{s.ec2Generator}
	case config.CloudPlatformGCE:
		return &CloudGenerator{s.gceGenerator}
	case config.CloudPlatformAzureVM:
		return &CloudGenerator{s.azureVMGenerator}
	}

	var wg sync.WaitGroup
	gCh := make(chan *CloudGenerator, 3)

	// cancelable context
	ctx, cancel := context.WithCancel(context.Background())

	wg.Add(3)
	go func() {
		if s.ec2Generator.IsEC2(ctx) {
			gCh <- &CloudGenerator{s.ec2Generator}
			cancel()
		}
		wg.Done()
	}()
	go func() {
		if s.gceGenerator.IsGCE(ctx) {
			gCh <- &CloudGenerator{s.gceGenerator}
			cancel()
		}
		wg.Done()
	}()
	go func() {
		if s.azureVMGenerator.IsAzureVM(ctx) {
			gCh <- &CloudGenerator{s.azureVMGenerator}
			cancel()
		}
		wg.Done()
	}()

	go func() {
		wg.Wait()
		// close so that `<-gCh` will receive nul
		close(gCh)
	}()

	return <-gCh
}

// CloudGeneratorSuggester suggests suitable CloudGenerator
var CloudGeneratorSuggester *cloudGeneratorSuggester

func init() {
	ec2BaseURL, _ = url.Parse("http://169.254.169.254/latest/meta-data")
	gceMetaURL, _ = url.Parse("http://metadata.google.internal./computeMetadata/v1/?recursive=true")
	azureVMBaseURL, _ = url.Parse("http://169.254.169.254/metadata/instance")

	CloudGeneratorSuggester = &cloudGeneratorSuggester{
		ec2Generator:     &EC2Generator{ec2BaseURL},
		gceGenerator:     &GCEGenerator{gceMetaURL},
		azureVMGenerator: &AzureVMGenerator{azureVMBaseURL},
	}
}

var timeout = 3 * time.Second

func httpCli() *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// don't use HTTP_PROXY when requesting cloud instance metadata APIs
			Proxy: nil,
		},
	}
}

// EC2Generator meta generator for EC2
type EC2Generator struct {
	baseURL *url.URL
}

// IsEC2 checks current environment is EC2 or not
func (g *EC2Generator) IsEC2(ctx context.Context) bool {
	// implementation varies between OSs. see isec2_XXX.go
	return isEC2(ctx)
}

// Generate collects metadata from cloud platform.
func (g *EC2Generator) Generate() (*mackerel.Cloud, error) {
	cl := httpCli()

	metadataKeys := []string{
		"instance-id",
		"instance-type",
		"placement/availability-zone",
		"security-groups",
		"ami-id",
		"hostname",
		"local-hostname",
		"public-hostname",
		"local-ipv4",
		"public-ipv4",
		"reservation-id",
	}

	metadata := make(map[string]string)

	for _, key := range metadataKeys {
		resp, err := cl.Get(g.baseURL.String() + "/" + key)
		if err != nil {
			cloudLogger.Debugf("This host may not be running on EC2. Error while reading '%s'", key)
			return nil, nil
		}
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				cloudLogger.Errorf("Results of requesting metadata cannot be read: '%s'", err)
				break
			}
			metadata[key] = string(body)
			cloudLogger.Debugf("results %s:%s", key, string(body))
		} else {
			cloudLogger.Debugf("Status code of the result of requesting metadata '%s' is '%d'", key, resp.StatusCode)
		}
	}

	return &mackerel.Cloud{Provider: "ec2", MetaData: metadata}, nil
}

// SuggestCustomIdentifier suggests the identifier of the EC2 instance
func (g *EC2Generator) SuggestCustomIdentifier() (string, error) {
	identifier := ""
	err := retry.Retry(3, 2*time.Second, func() error {
		cl := httpCli()
		key := "instance-id"
		resp, err := cl.Get(g.baseURL.String() + "/" + key)
		if err != nil {
			return fmt.Errorf("error while retrieving instance-id")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("failed to request instance-id. response code: %d", resp.StatusCode)
		}
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("results of requesting instance-id cannot be read: '%s'", err)
		}
		instanceID := string(body)
		if instanceID == "" {
			return fmt.Errorf("invalid instance id")
		}
		identifier = instanceID + ".ec2.amazonaws.com"
		return nil
	})
	return identifier, err
}

// GCEGenerator generate for GCE
type GCEGenerator struct {
	metaURL *url.URL
}

func requestGCEMeta(ctx context.Context) ([]byte, error) {
	cl := httpCli()
	req, err := http.NewRequest("GET", gceMetaURL.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := cl.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to request gce meta. response code: %d", resp.StatusCode)
	}
	return ioutil.ReadAll(resp.Body)
}

// IsGCE checks current environment is GCE or not
func (g *GCEGenerator) IsGCE(ctx context.Context) bool {
	err := retry.WithContext(ctx, 2, 2*time.Second, func() error {
		_, err := requestGCEMeta(ctx)
		return err
	})
	return err == nil
}

// Generate collects metadata from cloud platform.
func (g *GCEGenerator) Generate() (*mackerel.Cloud, error) {
	bytes, err := requestGCEMeta(context.Background())
	if err != nil {
		return nil, err
	}
	var data gceMeta
	json.Unmarshal(bytes, &data)
	return data.toGeneratorResults(), nil
}

type gceInstance struct {
	Zone         string
	InstanceType string `json:"machineType"`
	Hostname     string
	InstanceID   uint64 `json:"id"`
}

type gceProject struct {
	ProjectID        string
	NumericProjectID uint64
}

type gceMeta struct {
	Instance *gceInstance
	Project  *gceProject
}

func (g gceMeta) toGeneratorMeta() map[string]string {
	meta := make(map[string]string)

	lastS := func(s string) string {
		ss := strings.Split(s, "/")
		return ss[len(ss)-1]
	}

	if ins := g.Instance; ins != nil {
		meta["hostname"] = ins.Hostname
		meta["instance-id"] = fmt.Sprint(ins.InstanceID)
		meta["instance-type"] = lastS(ins.InstanceType)
		meta["zone"] = lastS(ins.Zone)
	}

	if proj := g.Project; proj != nil {
		meta["projectId"] = proj.ProjectID
	}

	return meta
}

func (g gceMeta) toGeneratorResults() *mackerel.Cloud {
	return &mackerel.Cloud{Provider: "gce", MetaData: g.toGeneratorMeta()}
}

// SuggestCustomIdentifier for GCE is not implemented yet
func (g *GCEGenerator) SuggestCustomIdentifier() (string, error) {
	return "", nil
}

// AzureVMGenerator meta generator for Azure VM
type AzureVMGenerator struct {
	baseURL *url.URL
}

// IsAzureVM checks current environment is AzureVM or not
func (g *AzureVMGenerator) IsAzureVM(ctx context.Context) bool {
	isAzureVM := false
	err := retry.WithContext(ctx, 2, 2*time.Second, func() error {
		cl := httpCli()
		// '/vmId` is probably Azure VM specific URL
		req, err := http.NewRequest("GET", azureVMBaseURL.String()+"/compute/vmId?api-version=2017-04-02&format=text", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Metadata", "true")

		resp, err := cl.Do(req.WithContext(ctx))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		isAzureVM = resp.StatusCode == 200
		return nil
	})
	return err == nil && isAzureVM
}

// Generate collects metadata from cloud platform.
func (g *AzureVMGenerator) Generate() (*mackerel.Cloud, error) {
	metadataComputeKeys := map[string]string{
		"location":  "location",
		"offer":     "imageReferenceOffer",
		"osType":    "osSystemType",
		"publisher": "imageReferencePublisher",
		"sku":       "imageReferenceSku",
		"vmId":      "vmID",
		"vmSize":    "virtualMachineSizeType",
	}

	ipAddressKeys := map[string]string{
		"privateIpAddress": "privateIpAddress",
		"publicIpAddress":  "publicIpAddress",
	}

	metadata := make(map[string]string)
	metadata = retrieveAzureVMMetadata(metadata, g.baseURL.String(), "/compute/", metadataComputeKeys)
	metadata = retrieveAzureVMMetadata(metadata, g.baseURL.String(), "/network/interface/0/ipv4/ipAddress/0/", ipAddressKeys)

	return &mackerel.Cloud{Provider: "AzureVM", MetaData: metadata}, nil
}

func retrieveAzureVMMetadata(metadataMap map[string]string, baseURL string, urlSuffix string, keys map[string]string) map[string]string {
	cl := httpCli()

	for key, value := range keys {
		req, err := http.NewRequest("GET", baseURL+urlSuffix+key+"?api-version=2017-04-02&format=text", nil)
		if err != nil {
			cloudLogger.Debugf("This host may not be running on Azure VM. Error while reading '%s'", key)
			return metadataMap
		}

		req.Header.Set("Metadata", "true")

		resp, err := cl.Do(req)
		if err != nil {
			cloudLogger.Debugf("This host may not be running on Azure VM. Error while reading '%s'", key)
			return metadataMap
		}
		defer resp.Body.Close()

		if resp.StatusCode == 200 {
			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				cloudLogger.Errorf("Results of requesting metadata cannot be read: '%s'", err)
				break
			}
			metadataMap[value] = string(body)
			cloudLogger.Debugf("results %s:%s", key, string(body))
		} else {
			cloudLogger.Debugf("Status code of the result of requesting metadata '%s' is '%d'", key, resp.StatusCode)
		}
	}
	return metadataMap
}

// SuggestCustomIdentifier suggests the identifier of the Azure VM instance
func (g *AzureVMGenerator) SuggestCustomIdentifier() (string, error) {
	identifier := ""
	err := retry.Retry(3, 2*time.Second, func() error {
		cl := httpCli()
		req, err := http.NewRequest("GET", azureVMBaseURL.String()+"/compute/vmId?api-version=2017-04-02&format=text", nil)
		if err != nil {
			return fmt.Errorf("error while retrieving vmId")
		}
		req.Header.Set("Metadata", "true")

		resp, err := cl.Do(req)
		if err != nil {
			return fmt.Errorf("error while retrieving vmId")
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return fmt.Errorf("failed to request vmId. response code: %d", resp.StatusCode)
		}
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("results of requesting vmId cannot be read: '%s'", err)
		}
		instanceID := string(body)
		if instanceID == "" {
			return fmt.Errorf("invalid instance id")
		}
		identifier = instanceID + ".virtual_machine.azure.microsoft.com"
		return nil
	})
	return identifier, err
}
