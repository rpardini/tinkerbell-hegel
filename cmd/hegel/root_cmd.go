package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	_ "net/http/pprof" //nolint:gosec // G108: Profiling endpoint is automatically exposed on /debug/pprof

	"github.com/equinix-labs/otel-init-go/otelinit"
	"github.com/packethost/pkg/log"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/tinkerbell/hegel/internal/datamodel"
	"github.com/tinkerbell/hegel/internal/hardware"
	"github.com/tinkerbell/hegel/internal/http"
	"github.com/tinkerbell/hegel/internal/http/handler"
	"github.com/tinkerbell/hegel/internal/metrics"
	"github.com/tinkerbell/hegel/internal/xff"
)

const longHelp = `
Run a Hegel server.

Each CLI argument has a corresponding environment variable in the form of the CLI argument prefixed 
with HEGEL. If both the flag and environment variable form are specified, the flag form takes 
precedence.

Examples
  --http-port          HEGEL_HTTP_PORT
  --trusted-proxies	   HEGEL_TRUSTED_PROXIES
`

// EnvNamePrefix defines the environment variable prefix required for all environment configuration.
const EnvNamePrefix = "HEGEL"

// RootCommandOptions encompasses all the configurability of the RootCommand.
type RootCommandOptions struct {
	DataModel      string `mapstructure:"data-model"`
	TrustedProxies string `mapstructure:"trusted-proxies"`

	HTTPPort int `mapstructure:"http-port"`

	KubernetesAPIURL string `mapstructure:"kubernetes"`
	Kubeconfig       string `mapstructure:"kubeconfig"`
	KubeNamespace    string `mapstructure:"kube-namespace"`

	// Hidden CLI flags.
	HegelAPI bool `mapstructure:"hegel-api"`
}

func (o RootCommandOptions) GetDataModel() datamodel.DataModel {
	return datamodel.DataModel(o.DataModel)
}

// GetAPI returns the API identifier for route configuration. If the --hegel-api flag is set, it
// returns handler.Hegel, otherwise it returns handler.EC2.
func (o RootCommandOptions) GetAPI() handler.API {
	if o.HegelAPI {
		return handler.Hegel
	}

	return handler.EC2
}

// RootCommand is the root command that represents the entrypoint to Hegel.
type RootCommand struct {
	*cobra.Command
	vpr  *viper.Viper
	Opts RootCommandOptions
}

// NewRootCommand creates new RootCommand instance.
func NewRootCommand() (*RootCommand, error) {
	rootCmd := &RootCommand{
		Command: &cobra.Command{
			Use:          os.Args[0],
			Long:         longHelp,
			SilenceUsage: true,
		},
	}

	rootCmd.PreRunE = rootCmd.PreRun
	rootCmd.RunE = rootCmd.Run
	rootCmd.Flags().SortFlags = false // Print flag help in the order they're specified.

	// Ensure keys with `-` use `_` for env keys else Viper won't match them.
	rootCmd.vpr = viper.NewWithOptions(viper.EnvKeyReplacer(strings.NewReplacer("-", "_")))
	rootCmd.vpr.SetEnvPrefix(EnvNamePrefix)

	if err := rootCmd.configureFlags(); err != nil {
		return nil, err
	}

	return rootCmd, nil
}

// PreRun satisfies cobra.Command.PreRunE and unmarshalls. Its responsible for populating c.Opts.
func (c *RootCommand) PreRun(*cobra.Command, []string) error {
	return c.vpr.Unmarshal(&c.Opts)
}

// Run executes Hegel.
func (c *RootCommand) Run(cmd *cobra.Command, _ []string) error {
	logger, err := log.Init("github.com/tinkerbell/hegel")
	if err != nil {
		return errors.Errorf("initialize logger: %v", err)
	}
	defer logger.Close()

	logger.With("opts", fmt.Sprintf("%+v", c.Opts)).Info("Root command options")

	ctx, otelShutdown := otelinit.InitOpenTelemetry(cmd.Context(), "hegel")
	defer otelShutdown(ctx)

	metrics.State.Set(metrics.Initializing)

	backend, err := hardware.NewClient(hardware.ClientConfig{
		Model:         c.Opts.GetDataModel(),
		KubeAPI:       c.Opts.KubernetesAPIURL,
		Kubeconfig:    c.Opts.Kubeconfig,
		KubeNamespace: c.Opts.KubeNamespace,
	})
	if err != nil {
		return errors.Errorf("create client: %v", err)
	}

	handlr, err := handler.New(logger, c.Opts.GetAPI(), backend)
	if err != nil {
		return err
	}

	// Add an X-Forward-For middleware for proxies.
	proxies, err := xff.Parse(c.Opts.TrustedProxies)
	if err != nil {
		return err
	}

	handlr, err = xff.Middleware(handlr, proxies)
	if err != nil {
		return err
	}

	// Listen for signals to gracefully shutdown.
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()

	return http.Serve(ctx, logger, fmt.Sprintf(":%v", c.Opts.HTTPPort), handlr)
}

func (c *RootCommand) configureFlags() error {
	c.Flags().String("data-model", string(datamodel.TinkServer), "The back-end data source: [\"1\", \"kubernetes\"] (1 indicates tink server)")

	c.Flags().Int("http-port", 50061, "Port to listen on for HTTP requests")

	c.Flags().String("kubeconfig", "", "Path to a kubeconfig file")
	c.Flags().String("kubernetes", "", "URL of the Kubernetes API Server")
	c.Flags().String("kube-namespace", "", "The Kubernetes namespace to target; defaults to the service account")

	c.Flags().String("trusted-proxies", "", "A commma separated list of allowed peer IPs and/or CIDR blocks to replace with X-Forwarded-For")

	c.Flags().Bool("hegel-api", false, "Toggle to true to enable Hegel's new experimental API. Default is false.")
	if err := c.Flags().MarkHidden("hegel-api"); err != nil {
		return err
	}

	if err := c.vpr.BindPFlags(c.Flags()); err != nil {
		return err
	}

	var err error
	c.Flags().VisitAll(func(f *pflag.Flag) {
		if err != nil {
			return
		}
		err = c.vpr.BindEnv(f.Name)
	})

	return err
}