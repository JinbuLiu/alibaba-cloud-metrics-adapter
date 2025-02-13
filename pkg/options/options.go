package options

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/AliyunContainerService/alibaba-cloud-metrics-adapter/pkg/utils"
	"io/ioutil"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"net/http"
	"net/url"
	basecmd "sigs.k8s.io/custom-metrics-apiserver/pkg/cmd"
	prom "sigs.k8s.io/prometheus-adapter/pkg/client"
	cfg "sigs.k8s.io/prometheus-adapter/pkg/config"
	"strings"
	"time"

	"k8s.io/client-go/transport"
	"k8s.io/klog/v2"
)

type AlibabaMetricsAdapterOptions struct {
	basecmd.AdapterBase
	// PrometheusURL is the URL describing how to connect to Prometheus.  Query parameters configure connection options.
	PrometheusURL string
	// PrometheusAuthInCluster enables using the auth details from the in-cluster kubeconfig to connect to Prometheus
	PrometheusAuthInCluster bool
	// PrometheusAuthConf is the kubeconfig file that contains auth details used to connect to Prometheus
	PrometheusAuthConf string
	// PrometheusCAFile points to the file containing the ca-root for connecting with Prometheus
	PrometheusCAFile string
	// PrometheusTokenFile points to the file that contains the bearer token when connecting with Prometheus
	PrometheusTokenFile string
	// PrometheusHeaders is a k=v list of headers to set on requests to PrometheusURL
	PrometheusHeaders []string
	// AdapterConfigFile points to the file containing the metrics discovery configuration.
	AdapterConfigFile string
	// MetricsRelistInterval is the interval at which to relist the set of available metrics
	MetricsRelistInterval time.Duration
	// MetricsMaxAge is the period to query available metrics for
	MetricsMaxAge time.Duration

	MetricsConfig *cfg.MetricsDiscoveryConfig
}

func (cmd *AlibabaMetricsAdapterOptions) AddFlags() {
	cmd.Flags().StringVar(&cmd.PrometheusURL, "prometheus-url", cmd.PrometheusURL,
		"URL for connecting to Prometheus.")
	cmd.Flags().BoolVar(&cmd.PrometheusAuthInCluster, "prometheus-auth-incluster", cmd.PrometheusAuthInCluster,
		"use auth details from the in-cluster kubeconfig when connecting to prometheus.")
	cmd.Flags().StringVar(&cmd.PrometheusAuthConf, "prometheus-auth-config", cmd.PrometheusAuthConf,
		"kubeconfig file used to configure auth when connecting to Prometheus.")
	cmd.Flags().StringVar(&cmd.PrometheusCAFile, "prometheus-ca-file", cmd.PrometheusCAFile,
		"Optional CA file to use when connecting with Prometheus")
	cmd.Flags().StringVar(&cmd.PrometheusTokenFile, "prometheus-token-file", cmd.PrometheusTokenFile,
		"Optional file containing the bearer token to use when connecting with Prometheus")
	cmd.Flags().StringArrayVar(&cmd.PrometheusHeaders, "prometheus-header", cmd.PrometheusHeaders,
		"Optional header to set on requests to prometheus-url. Can be repeated")
	cmd.Flags().StringVar(&cmd.AdapterConfigFile, "config", cmd.AdapterConfigFile,
		"Configuration file containing details of how to transform between Prometheus metrics "+
			"and custom metrics API resources")
	cmd.Flags().DurationVar(&cmd.MetricsRelistInterval, "metrics-relist-interval", cmd.MetricsRelistInterval, ""+
		"interval at which to re-list the set of all available metrics from Prometheus")
	cmd.Flags().DurationVar(&cmd.MetricsMaxAge, "metrics-max-age", cmd.MetricsMaxAge, ""+
		"period for which to query the set of available metrics from Prometheus")
}

func (cmd *AlibabaMetricsAdapterOptions) LoadConfig() error {
	// load metrics discovery configuration
	if cmd.AdapterConfigFile == "" {
		return fmt.Errorf("no metrics discovery configuration file specified (make sure to use --config)")
	}

	metricsConfig, err := cfg.FromFile(cmd.AdapterConfigFile)
	if err != nil {
		return fmt.Errorf("unable to load metrics discovery configuration: %v", err)
	}

	cmd.MetricsConfig = metricsConfig

	return nil
}

func (cmd *AlibabaMetricsAdapterOptions) MakePromClient() (prom.Client, error) {
	baseURL, err := url.Parse(cmd.PrometheusURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Prometheus URL %q: %v", baseURL, err)
	}

	var httpClient *http.Client

	if cmd.PrometheusCAFile != "" {
		prometheusCAClient, err := makePrometheusCAClient(cmd.PrometheusCAFile)
		if err != nil {
			return nil, err
		}
		httpClient = prometheusCAClient
		klog.Info("successfully loaded ca from file")
	} else {
		kubeconfigHTTPClient, err := makeKubeconfigHTTPClient(cmd.PrometheusAuthInCluster, cmd.PrometheusAuthConf)
		if err != nil {
			return nil, err
		}
		httpClient = kubeconfigHTTPClient
		klog.Info("successfully using in-cluster auth")
	}

	if cmd.PrometheusTokenFile != "" {
		data, err := ioutil.ReadFile(cmd.PrometheusTokenFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read prometheus-token-file: %v", err)
		}
		httpClient.Transport = transport.NewBearerAuthRoundTripper(string(data), httpClient.Transport)
	}

	genericPromClient := prom.NewGenericAPIClient(httpClient, baseURL, parseHeaderArgs(cmd.PrometheusHeaders))
	instrumentedGenericPromClient := utils.InstrumentGenericAPIClient(genericPromClient, baseURL.String())
	return prom.NewClientForAPI(instrumentedGenericPromClient), nil
}

func makePrometheusCAClient(caFilename string) (*http.Client, error) {
	data, err := ioutil.ReadFile(caFilename)
	if err != nil {
		return nil, fmt.Errorf("failed to read prometheus-ca-file: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("no certs found in prometheus-ca-file")
	}

	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: pool,
			},
		},
	}, nil
}

// makeKubeconfigHTTPClient constructs an HTTP for connecting with the given auth options.
func makeKubeconfigHTTPClient(inClusterAuth bool, kubeConfigPath string) (*http.Client, error) {
	// make sure we're not trying to use two different sources of auth
	if inClusterAuth && kubeConfigPath != "" {
		return nil, fmt.Errorf("may not use both in-cluster auth and an explicit kubeconfig at the same time")
	}

	// return the default client if we're using no auth
	if !inClusterAuth && kubeConfigPath == "" {
		return http.DefaultClient, nil
	}

	var authConf *rest.Config
	if kubeConfigPath != "" {
		var err error
		loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeConfigPath}
		loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
		authConf, err = loader.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("unable to construct  auth configuration from %q for connecting to Prometheus: %v", kubeConfigPath, err)
		}
	} else {
		var err error
		authConf, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("unable to construct in-cluster auth configuration for connecting to Prometheus: %v", err)
		}
	}
	tr, err := rest.TransportFor(authConf)
	if err != nil {
		return nil, fmt.Errorf("unable to construct client transport for connecting to Prometheus: %v", err)
	}
	return &http.Client{Transport: tr}, nil
}

func parseHeaderArgs(args []string) http.Header {
	headers := make(http.Header, len(args))
	for _, h := range args {
		parts := strings.SplitN(h, "=", 2)
		value := ""
		if len(parts) > 1 {
			value = parts[1]
		}
		headers.Add(parts[0], value)
	}
	return headers
}

func NewAlibabaMetricsAdapterOptions() *AlibabaMetricsAdapterOptions {
	opts := &AlibabaMetricsAdapterOptions{
		PrometheusURL:         "http://ack-prometheus-operator-prometheus.monitoring.svc:9090",
		MetricsRelistInterval: 10 * time.Minute,
		MetricsMaxAge:         20 * time.Minute,
		MetricsConfig:         new(cfg.MetricsDiscoveryConfig),
	}
	return opts
}
