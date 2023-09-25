/*
   Copyright 2020 Docker Compose CLI authors

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

package compose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/compose-spec/compose-go/cli"
	"github.com/compose-spec/compose-go/dotenv"
	"github.com/compose-spec/compose-go/types"
	composegoutils "github.com/compose-spec/compose-go/utils"
	"github.com/docker/buildx/util/logutil"
	buildx "github.com/docker/buildx/util/progress"
	dockercli "github.com/docker/cli/cli"
	"github.com/docker/cli/cli-plugins/manager"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v2/pkg/remote"
	"github.com/morikuni/aec"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/docker/compose/v2/cmd/formatter"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
	ui "github.com/docker/compose/v2/pkg/progress"
	"github.com/docker/compose/v2/pkg/utils"
)

const (
	// ComposeParallelLimit set the limit running concurrent operation on docker engine
	ComposeParallelLimit = "COMPOSE_PARALLEL_LIMIT"
	// ComposeProjectName define the project name to be used, instead of guessing from parent directory
	ComposeProjectName = "COMPOSE_PROJECT_NAME"
	// ComposeCompatibility try to mimic compose v1 as much as possible
	ComposeCompatibility = "COMPOSE_COMPATIBILITY"
	// ComposeRemoveOrphans remove “orphaned" containers, i.e. containers tagged for current project but not declared as service
	ComposeRemoveOrphans = "COMPOSE_REMOVE_ORPHANS"
	// ComposeIgnoreOrphans ignore "orphaned" containers
	ComposeIgnoreOrphans = "COMPOSE_IGNORE_ORPHANS"
)

// Command defines a compose CLI command as a func with args
type Command func(context.Context, []string) error

// CobraCommand defines a cobra command function
type CobraCommand func(context.Context, *cobra.Command, []string) error

// AdaptCmd adapt a CobraCommand func to cobra library
func AdaptCmd(fn CobraCommand) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		contextString := fmt.Sprintf("%s", ctx)
		if !strings.HasSuffix(contextString, ".WithCancel") { // need to handle cancel
			cancellableCtx, cancel := context.WithCancel(cmd.Context())
			ctx = cancellableCtx
			s := make(chan os.Signal, 1)
			signal.Notify(s, syscall.SIGTERM, syscall.SIGINT)
			go func() {
				<-s
				cancel()
			}()
		}
		err := fn(ctx, cmd, args)
		var composeErr compose.Error
		if api.IsErrCanceled(err) || errors.Is(ctx.Err(), context.Canceled) {
			err = dockercli.StatusError{
				StatusCode: 130,
				Status:     compose.CanceledStatus,
			}
		}
		if errors.As(err, &composeErr) {
			err = dockercli.StatusError{
				StatusCode: composeErr.GetMetricsFailureCategory().ExitCode,
				Status:     err.Error(),
			}
		}
		return err
	}
}

// Adapt a Command func to cobra library
func Adapt(fn Command) func(cmd *cobra.Command, args []string) error {
	return AdaptCmd(func(ctx context.Context, cmd *cobra.Command, args []string) error {
		return fn(ctx, args)
	})
}

type ProjectOptions struct {
	ProjectName   string
	Profiles      []string
	ConfigPaths   []string
	WorkDir       string
	ProjectDir    string
	EnvFiles      []string
	Compatibility bool
	Progress      string
	Offline       bool
}

// ProjectFunc does stuff within a types.Project
type ProjectFunc func(ctx context.Context, project *types.Project) error

// ProjectServicesFunc does stuff within a types.Project and a selection of services
type ProjectServicesFunc func(ctx context.Context, project *types.Project, services []string) error

// WithProject creates a cobra run command from a ProjectFunc based on configured project options and selected services
func (o *ProjectOptions) WithProject(fn ProjectFunc, dockerCli command.Cli) func(cmd *cobra.Command, args []string) error {
	return o.WithServices(dockerCli, func(ctx context.Context, project *types.Project, services []string) error {
		return fn(ctx, project)
	})
}

// WithServices creates a cobra run command from a ProjectFunc based on configured project options and selected services
func (o *ProjectOptions) WithServices(dockerCli command.Cli, fn ProjectServicesFunc) func(cmd *cobra.Command, args []string) error {
	return Adapt(func(ctx context.Context, args []string) error {
		options := []cli.ProjectOptionsFn{
			cli.WithResolvedPaths(true),
			cli.WithDiscardEnvFile,
			cli.WithContext(ctx),
		}

		project, err := o.ToProject(dockerCli, args, options...)
		if err != nil {
			return err
		}

		return fn(ctx, project, args)
	})
}

func (o *ProjectOptions) addProjectFlags(f *pflag.FlagSet) {
	f.StringArrayVar(&o.Profiles, "profile", []string{}, "Specify a profile to enable")
	f.StringVarP(&o.ProjectName, "project-name", "p", "", "Project name")
	f.StringArrayVarP(&o.ConfigPaths, "file", "f", []string{}, "Compose configuration files")
	f.StringArrayVar(&o.EnvFiles, "env-file", nil, "Specify an alternate environment file.")
	f.StringVar(&o.ProjectDir, "project-directory", "", "Specify an alternate working directory\n(default: the path of the, first specified, Compose file)")
	f.StringVar(&o.WorkDir, "workdir", "", "DEPRECATED! USE --project-directory INSTEAD.\nSpecify an alternate working directory\n(default: the path of the, first specified, Compose file)")
	f.BoolVar(&o.Compatibility, "compatibility", false, "Run compose in backward compatibility mode")
	f.StringVar(&o.Progress, "progress", buildx.PrinterModeAuto, fmt.Sprintf(`Set type of progress output (%s)`, strings.Join(printerModes, ", ")))
	_ = f.MarkHidden("workdir")
}

func (o *ProjectOptions) projectOrName(dockerCli command.Cli, services ...string) (*types.Project, string, error) {
	name := o.ProjectName
	var project *types.Project
	if len(o.ConfigPaths) > 0 || o.ProjectName == "" {
		p, err := o.ToProject(dockerCli, services, cli.WithDiscardEnvFile)
		if err != nil {
			envProjectName := os.Getenv(ComposeProjectName)
			if envProjectName != "" {
				return nil, envProjectName, nil
			}
			return nil, "", err
		}
		project = p
		name = p.Name
	}
	return project, name, nil
}

func (o *ProjectOptions) toProjectName(dockerCli command.Cli) (string, error) {
	if o.ProjectName != "" {
		return o.ProjectName, nil
	}

	envProjectName := os.Getenv(ComposeProjectName)
	if envProjectName != "" {
		return envProjectName, nil
	}

	project, err := o.ToProject(dockerCli, nil)
	if err != nil {
		return "", err
	}
	return project.Name, nil
}

func (o *ProjectOptions) ToProject(dockerCli command.Cli, services []string, po ...cli.ProjectOptionsFn) (*types.Project, error) {
	if !o.Offline {
		var err error
		po, err = o.configureRemoteLoaders(dockerCli, po)
		if err != nil {
			return nil, err
		}
	}

	options, err := o.toProjectOptions(po...)
	if err != nil {
		return nil, compose.WrapComposeError(err)
	}

	if o.Compatibility || utils.StringToBool(options.Environment[ComposeCompatibility]) {
		api.Separator = "_"
	}

	project, err := cli.ProjectFromOptions(options)
	if err != nil {
		return nil, compose.WrapComposeError(err)
	}

	if project.Name == "" {
		return nil, errors.New("project name can't be empty. Use `--project-name` to set a valid name")
	}

	err = project.EnableServices(services...)
	if err != nil {
		return nil, err
	}

	for i, s := range project.Services {
		s.CustomLabels = map[string]string{
			api.ProjectLabel:     project.Name,
			api.ServiceLabel:     s.Name,
			api.VersionLabel:     api.ComposeVersion,
			api.WorkingDirLabel:  project.WorkingDir,
			api.ConfigFilesLabel: strings.Join(project.ComposeFiles, ","),
			api.OneoffLabel:      "False", // default, will be overridden by `run` command
		}
		if len(o.EnvFiles) != 0 {
			s.CustomLabels[api.EnvironmentFileLabel] = strings.Join(o.EnvFiles, ",")
		}
		project.Services[i] = s
	}

	project.WithoutUnnecessaryResources()

	err = project.ForServices(services)
	return project, err
}

func (o *ProjectOptions) configureRemoteLoaders(dockerCli command.Cli, po []cli.ProjectOptionsFn) ([]cli.ProjectOptionsFn, error) {
	enabled, err := remote.GitRemoteLoaderEnabled()
	if err != nil {
		return nil, err
	}
	if enabled {
		git, err := remote.NewGitRemoteLoader(o.Offline)
		if err != nil {
			return nil, err
		}
		po = append(po, cli.WithResourceLoader(git))
	}

	enabled, err = remote.OCIRemoteLoaderEnabled()
	if err != nil {
		return nil, err
	}
	if enabled {
		git, err := remote.NewOCIRemoteLoader(dockerCli, o.Offline)
		if err != nil {
			return nil, err
		}
		po = append(po, cli.WithResourceLoader(git))
	}
	return po, nil
}

func (o *ProjectOptions) toProjectOptions(po ...cli.ProjectOptionsFn) (*cli.ProjectOptions, error) {
	return cli.NewProjectOptions(o.ConfigPaths,
		append(po,
			cli.WithWorkingDirectory(o.ProjectDir),
			cli.WithOsEnv,
			cli.WithEnvFiles(o.EnvFiles...),
			cli.WithDotEnv,
			cli.WithConfigFileEnv,
			cli.WithDefaultConfigPath,
			cli.WithDefaultProfiles(o.Profiles...),
			cli.WithName(o.ProjectName))...)
}

// PluginName is the name of the plugin
const PluginName = "compose"

// RunningAsStandalone detects when running as a standalone program
func RunningAsStandalone() bool {
	return len(os.Args) < 2 || os.Args[1] != manager.MetadataSubcommandName && os.Args[1] != PluginName
}

// RootCommand returns the compose command with its child commands
func RootCommand(dockerCli command.Cli, backend api.Service) *cobra.Command { //nolint:gocyclo
	// filter out useless commandConn.CloseWrite warning message that can occur
	// when using a remote context that is unreachable: "commandConn.CloseWrite: commandconn: failed to wait: signal: killed"
	// https://github.com/docker/cli/blob/e1f24d3c93df6752d3c27c8d61d18260f141310c/cli/connhelper/commandconn/commandconn.go#L203-L215
	logrus.AddHook(logutil.NewFilter([]logrus.Level{
		logrus.WarnLevel,
	},
		"commandConn.CloseWrite:",
		"commandConn.CloseRead:",
	))

	opts := ProjectOptions{}
	var (
		ansi     string
		noAnsi   bool
		verbose  bool
		version  bool
		parallel int
		dryRun   bool
	)
	c := &cobra.Command{
		Short:            "Docker Compose",
		Long:             "Define and run multi-container applications with Docker.",
		Use:              PluginName,
		TraverseChildren: true,
		// By default (no Run/RunE in parent c) for typos in subcommands, cobra displays the help of parent c but exit(0) !
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if version {
				return versionCommand(dockerCli).Execute()
			}
			_ = cmd.Help()
			return dockercli.StatusError{
				StatusCode: compose.CommandSyntaxFailure.ExitCode,
				Status:     fmt.Sprintf("unknown docker command: %q", "compose "+args[0]),
			}
		},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			err := setEnvWithDotEnv(&opts)
			if err != nil {
				return err
			}
			parent := cmd.Root()
			if parent != nil {
				parentPrerun := parent.PersistentPreRunE
				if parentPrerun != nil {
					err := parentPrerun(cmd, args)
					if err != nil {
						return err
					}
				}
			}
			if noAnsi {
				if ansi != "auto" {
					return errors.New(`cannot specify DEPRECATED "--no-ansi" and "--ansi". Please use only "--ansi"`)
				}
				ansi = "never"
				fmt.Fprint(os.Stderr, "option '--no-ansi' is DEPRECATED ! Please use '--ansi' instead.\n")
			}
			if verbose {
				logrus.SetLevel(logrus.TraceLevel)
			}

			if v, ok := os.LookupEnv("COMPOSE_ANSI"); ok && !cmd.Flags().Changed("ansi") {
				ansi = v
			}

			formatter.SetANSIMode(dockerCli, ansi)

			if noColor, ok := os.LookupEnv("NO_COLOR"); ok && noColor != "" {
				ui.NoColor()
				formatter.SetANSIMode(dockerCli, formatter.Never)
			}

			switch ansi {
			case "never":
				ui.Mode = ui.ModePlain
			case "always":
				ui.Mode = ui.ModeTTY
			}

			switch opts.Progress {
			case ui.ModeAuto:
				ui.Mode = ui.ModeAuto
			case ui.ModeTTY:
				if ansi == "never" {
					return fmt.Errorf("can't use --progress tty while ANSI support is disabled")
				}
				ui.Mode = ui.ModeTTY
			case ui.ModePlain:
				if ansi == "always" {
					return fmt.Errorf("can't use --progress plain while ANSI support is forced")
				}
				ui.Mode = ui.ModePlain
			case ui.ModeQuiet, "none":
				ui.Mode = ui.ModeQuiet
			default:
				return fmt.Errorf("unsupported --progress value %q", opts.Progress)
			}

			if opts.WorkDir != "" {
				if opts.ProjectDir != "" {
					return errors.New(`cannot specify DEPRECATED "--workdir" and "--project-directory". Please use only "--project-directory" instead`)
				}
				opts.ProjectDir = opts.WorkDir
				fmt.Fprint(os.Stderr, aec.Apply("option '--workdir' is DEPRECATED at root level! Please use '--project-directory' instead.\n", aec.RedF))
			}
			for i, file := range opts.EnvFiles {
				if !filepath.IsAbs(file) {
					file, err = filepath.Abs(file)
					if err != nil {
						return err
					}
					opts.EnvFiles[i] = file
				}
			}

			composeCmd := cmd
			for {
				if composeCmd.Name() == PluginName {
					break
				}
				if !composeCmd.HasParent() {
					return fmt.Errorf("error parsing command line, expected %q", PluginName)
				}
				composeCmd = composeCmd.Parent()
			}

			if v, ok := os.LookupEnv(ComposeParallelLimit); ok && !composeCmd.Flags().Changed("parallel") {
				i, err := strconv.Atoi(v)
				if err != nil {
					return fmt.Errorf("%s must be an integer (found: %q)", ComposeParallelLimit, v)
				}
				parallel = i
			}
			if parallel > 0 {
				backend.MaxConcurrency(parallel)
			}
			ctx, err := backend.DryRunMode(cmd.Context(), dryRun)
			if err != nil {
				return err
			}
			cmd.SetContext(ctx)
			return nil
		},
	}

	c.AddCommand(
		upCommand(&opts, dockerCli, backend),
		downCommand(&opts, dockerCli, backend),
		startCommand(&opts, dockerCli, backend),
		restartCommand(&opts, dockerCli, backend),
		stopCommand(&opts, dockerCli, backend),
		psCommand(&opts, dockerCli, backend),
		listCommand(dockerCli, backend),
		logsCommand(&opts, dockerCli, backend),
		configCommand(&opts, dockerCli, backend),
		killCommand(&opts, dockerCli, backend),
		runCommand(&opts, dockerCli, backend),
		removeCommand(&opts, dockerCli, backend),
		execCommand(&opts, dockerCli, backend),
		pauseCommand(&opts, dockerCli, backend),
		unpauseCommand(&opts, dockerCli, backend),
		topCommand(&opts, dockerCli, backend),
		eventsCommand(&opts, dockerCli, backend),
		portCommand(&opts, dockerCli, backend),
		imagesCommand(&opts, dockerCli, backend),
		versionCommand(dockerCli),
		buildCommand(&opts, dockerCli, backend),
		pushCommand(&opts, dockerCli, backend),
		pullCommand(&opts, dockerCli, backend),
		createCommand(&opts, dockerCli, backend),
		copyCommand(&opts, dockerCli, backend),
		waitCommand(&opts, dockerCli, backend),
		scaleCommand(&opts, dockerCli, backend),
		watchCommand(&opts, dockerCli, backend),
		alphaCommand(&opts, dockerCli, backend),
	)

	c.Flags().SetInterspersed(false)
	opts.addProjectFlags(c.Flags())
	c.RegisterFlagCompletionFunc( //nolint:errcheck
		"project-name",
		completeProjectNames(backend),
	)
	c.RegisterFlagCompletionFunc( //nolint:errcheck
		"project-directory",
		func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return []string{}, cobra.ShellCompDirectiveFilterDirs
		},
	)
	c.RegisterFlagCompletionFunc( //nolint:errcheck
		"file",
		func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return []string{"yaml", "yml"}, cobra.ShellCompDirectiveFilterFileExt
		},
	)
	c.RegisterFlagCompletionFunc( //nolint:errcheck
		"profile",
		completeProfileNames(dockerCli, &opts),
	)

	c.Flags().StringVar(&ansi, "ansi", "auto", `Control when to print ANSI control characters ("never"|"always"|"auto")`)
	c.Flags().IntVar(&parallel, "parallel", -1, `Control max parallelism, -1 for unlimited`)
	c.Flags().BoolVarP(&version, "version", "v", false, "Show the Docker Compose version information")
	c.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "Execute command in dry run mode")
	c.Flags().MarkHidden("version") //nolint:errcheck
	c.Flags().BoolVar(&noAnsi, "no-ansi", false, `Do not print ANSI control characters (DEPRECATED)`)
	c.Flags().MarkHidden("no-ansi") //nolint:errcheck
	c.Flags().BoolVar(&verbose, "verbose", false, "Show more output")
	c.Flags().MarkHidden("verbose") //nolint:errcheck
	return c
}

func setEnvWithDotEnv(prjOpts *ProjectOptions) error {
	options, err := prjOpts.toProjectOptions()
	if err != nil {
		return compose.WrapComposeError(err)
	}
	workingDir, err := options.GetWorkingDir()
	if err != nil {
		return err
	}

	envFromFile, err := dotenv.GetEnvFromFile(composegoutils.GetAsEqualsMap(os.Environ()), workingDir, options.EnvFiles)
	if err != nil {
		return err
	}
	for k, v := range envFromFile {
		if _, ok := os.LookupEnv(k); !ok { // Precedence to OS Env
			if err := os.Setenv(k, v); err != nil {
				return err
			}
		}
	}
	return nil
}

var printerModes = []string{
	ui.ModeAuto,
	ui.ModeTTY,
	ui.ModePlain,
	ui.ModeQuiet,
}
