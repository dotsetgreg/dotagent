package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dotsetgreg/dotagent/pkg/skills"
	"github.com/spf13/cobra"
)

func executeCLI() error {
	root := buildRootCommand(true)
	if err := root.Execute(); err != nil {
		return err
	}
	return nil
}

func buildRootCommand(includeDocsCommand bool) *cobra.Command {
	var showVersion bool

	root := &cobra.Command{
		Use:   "dotagent",
		Short: "Personal AI agent with Discord gateway, tools, memory, and provider routing",
		Long: strings.TrimSpace(`dotagent is a lean, extensible agent runtime.

Use CLI commands to onboard, run local agent sessions, run the Discord gateway,
manage cron jobs, install skills, and manage executable toolpacks.`),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if showVersion {
				printVersion()
				return nil
			}
			_ = cmd.Help()
			return fmt.Errorf("a subcommand is required")
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.Flags().BoolVarP(&showVersion, "version", "v", false, "Show build/version metadata")

	root.AddCommand(newOnboardCommand())
	root.AddCommand(newAgentCommand())
	root.AddCommand(newGatewayCommand())
	root.AddCommand(newStatusCommand())
	root.AddCommand(newCronCommand())
	root.AddCommand(newSkillsCommand())
	root.AddCommand(newToolpacksCommand())
	root.AddCommand(newVersionCommand())

	if includeDocsCommand {
		docsCmd := newDocsCommand(func() *cobra.Command { return buildRootCommand(false) })
		root.AddCommand(docsCmd)
	}

	return root
}

func runLegacyWithArgs(args []string, fn func()) error {
	original := os.Args
	os.Args = append([]string{original[0]}, args...)
	defer func() {
		os.Args = original
	}()
	fn()
	return nil
}

func newOnboardCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "onboard",
		Short:   "Initialize ~/.dotagent config and workspace templates",
		Long:    "Create default configuration and workspace template files for a new dotagent installation.",
		Example: "  dotagent onboard",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLegacyWithArgs([]string{"onboard"}, onboard)
		},
	}
}

func newAgentCommand() *cobra.Command {
	var (
		message string
		session string
		debug   bool
	)

	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run direct local chat with the agent (CLI mode)",
		Long:  "Run an interactive local agent session or send one-shot messages without Discord.",
		Example: strings.Join([]string{
			"  dotagent agent",
			"  dotagent agent --session cli:workspace",
			"  dotagent agent --message \"summarize my TODOs\"",
		}, "\n"),
		RunE: func(cmd *cobra.Command, args []string) error {
			legacyArgs := []string{"agent"}
			if debug {
				legacyArgs = append(legacyArgs, "--debug")
			}
			if strings.TrimSpace(message) != "" {
				legacyArgs = append(legacyArgs, "--message", message)
			}
			if strings.TrimSpace(session) != "" {
				legacyArgs = append(legacyArgs, "--session", session)
			}
			return runLegacyWithArgs(legacyArgs, agentCmd)
		},
	}

	cmd.Flags().StringVarP(&message, "message", "m", "", "One-shot prompt to send to the agent")
	cmd.Flags().StringVarP(&session, "session", "s", "cli:default", "Session key for continuity")
	cmd.Flags().BoolVarP(&debug, "debug", "d", false, "Enable debug logging")

	return cmd
}

func newGatewayCommand() *cobra.Command {
	var debug bool

	cmd := &cobra.Command{
		Use:     "gateway",
		Short:   "Run the Discord gateway + health server",
		Long:    "Start channel adapters, memory-backed agent loop, cron service, and heartbeat worker.",
		Example: "  dotagent gateway --debug",
		RunE: func(cmd *cobra.Command, args []string) error {
			legacyArgs := []string{"gateway"}
			if debug {
				legacyArgs = append(legacyArgs, "--debug")
			}
			return runLegacyWithArgs(legacyArgs, gatewayCmd)
		},
	}

	cmd.Flags().BoolVarP(&debug, "debug", "d", false, "Enable debug logging")
	return cmd
}

func newStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "status",
		Short:   "Show configuration, provider, and runtime readiness",
		Example: "  dotagent status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLegacyWithArgs([]string{"status"}, statusCmd)
		},
	}
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "Show build/version metadata",
		Example: "  dotagent version",
		RunE: func(cmd *cobra.Command, args []string) error {
			printVersion()
			return nil
		},
	}
}

func newCronCommand() *cobra.Command {
	cronRoot := &cobra.Command{
		Use:   "cron",
		Short: "Manage scheduled jobs",
		Long:  "Create and manage recurring or cron-expression based jobs for the agent.",
	}

	cronRoot.AddCommand(&cobra.Command{
		Use:     "list",
		Short:   "List scheduled jobs",
		Example: "  dotagent cron list",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLegacyWithArgs([]string{"cron", "list"}, cronCmd)
		},
	})

	var (
		name    string
		message string
		every   int64
		expr    string
		deliver bool
		to      string
		channel string
	)

	add := &cobra.Command{
		Use:   "add",
		Short: "Add a scheduled job",
		Long:  "Add a recurring job with either --every (seconds) or --cron expression.",
		Example: strings.Join([]string{
			"  dotagent cron add --name backup --message \"run backup\" --every 3600",
			"  dotagent cron add --name digest --message \"send daily digest\" --cron '0 9 * * *' --deliver --channel discord --to 1234",
		}, "\n"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("--name is required")
			}
			if strings.TrimSpace(message) == "" {
				return fmt.Errorf("--message is required")
			}
			if every <= 0 && strings.TrimSpace(expr) == "" {
				return fmt.Errorf("either --every or --cron must be provided")
			}
			if every > 0 && strings.TrimSpace(expr) != "" {
				return fmt.Errorf("--every and --cron are mutually exclusive")
			}

			legacyArgs := []string{"cron", "add", "--name", name, "--message", message}
			if every > 0 {
				legacyArgs = append(legacyArgs, "--every", strconv.FormatInt(every, 10))
			}
			if strings.TrimSpace(expr) != "" {
				legacyArgs = append(legacyArgs, "--cron", expr)
			}
			if deliver {
				legacyArgs = append(legacyArgs, "--deliver")
			}
			if strings.TrimSpace(to) != "" {
				legacyArgs = append(legacyArgs, "--to", to)
			}
			if strings.TrimSpace(channel) != "" {
				legacyArgs = append(legacyArgs, "--channel", channel)
			}
			return runLegacyWithArgs(legacyArgs, cronCmd)
		},
	}

	add.Flags().StringVarP(&name, "name", "n", "", "Job name")
	add.Flags().StringVarP(&message, "message", "m", "", "Message payload for the job")
	add.Flags().Int64VarP(&every, "every", "e", 0, "Run every N seconds")
	add.Flags().StringVarP(&expr, "cron", "c", "", "Cron expression (e.g. '0 9 * * *')")
	add.Flags().BoolVarP(&deliver, "deliver", "d", false, "Deliver result back to a channel target")
	add.Flags().StringVar(&to, "to", "", "Recipient/chat target")
	add.Flags().StringVar(&channel, "channel", "", "Channel name for delivery")
	cronRoot.AddCommand(add)

	remove := &cobra.Command{
		Use:     "remove <job_id>",
		Aliases: []string{"rm", "delete"},
		Short:   "Remove a scheduled job",
		Args:    cobra.ExactArgs(1),
		Example: "  dotagent cron remove job_abc123",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLegacyWithArgs([]string{"cron", "remove", args[0]}, cronCmd)
		},
	}
	cronRoot.AddCommand(remove)

	enable := &cobra.Command{
		Use:     "enable <job_id>",
		Short:   "Enable a disabled job",
		Args:    cobra.ExactArgs(1),
		Example: "  dotagent cron enable job_abc123",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLegacyWithArgs([]string{"cron", "enable", args[0]}, cronCmd)
		},
	}
	cronRoot.AddCommand(enable)

	disable := &cobra.Command{
		Use:     "disable <job_id>",
		Short:   "Disable a job",
		Args:    cobra.ExactArgs(1),
		Example: "  dotagent cron disable job_abc123",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLegacyWithArgs([]string{"cron", "disable", args[0]}, cronCmd)
		},
	}
	cronRoot.AddCommand(disable)

	return cronRoot
}

func newSkillsCommand() *cobra.Command {
	skillsRoot := &cobra.Command{
		Use:   "skills",
		Short: "Install, remove, search, and inspect skills",
	}

	skillsRoot.AddCommand(&cobra.Command{
		Use:     "list",
		Short:   "List installed skills",
		Example: "  dotagent skills list",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLegacyWithArgs([]string{"skills", "list"}, func() {
				cfg, err := loadConfig()
				if err != nil {
					fmt.Printf("Error loading config: %v\n", err)
					os.Exit(1)
				}
				workspace := cfg.WorkspacePath()
				globalDir := filepath.Dir(getConfigPath())
				globalSkillsDir := filepath.Join(globalDir, "skills")
				builtinSkillsDir := filepath.Join(globalDir, "dotagent", "skills")
				loader := skills.NewSkillsLoader(workspace, globalSkillsDir, builtinSkillsDir)
				skillsListCmd(loader)
			})
		},
	})

	install := &cobra.Command{
		Use:     "install <owner/repo-or-path>",
		Short:   "Install a skill from GitHub",
		Args:    cobra.ExactArgs(1),
		Example: "  dotagent skills install dotsetgreg/dotagent-skills/weather",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLegacyWithArgs([]string{"skills", "install", args[0]}, func() {
				cfg, err := loadConfig()
				if err != nil {
					fmt.Printf("Error loading config: %v\n", err)
					os.Exit(1)
				}
				installer := skills.NewSkillInstaller(cfg.WorkspacePath())
				skillsInstallCmd(installer)
			})
		},
	}
	skillsRoot.AddCommand(install)

	remove := &cobra.Command{
		Use:     "remove <skill>",
		Aliases: []string{"uninstall"},
		Short:   "Remove an installed skill",
		Args:    cobra.ExactArgs(1),
		Example: "  dotagent skills remove weather",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLegacyWithArgs([]string{"skills", "remove", args[0]}, func() {
				cfg, err := loadConfig()
				if err != nil {
					fmt.Printf("Error loading config: %v\n", err)
					os.Exit(1)
				}
				installer := skills.NewSkillInstaller(cfg.WorkspacePath())
				skillsRemoveCmd(installer, args[0])
			})
		},
	}
	skillsRoot.AddCommand(remove)

	search := &cobra.Command{
		Use:     "search",
		Short:   "List available curated skills",
		Example: "  dotagent skills search",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLegacyWithArgs([]string{"skills", "search"}, func() {
				cfg, err := loadConfig()
				if err != nil {
					fmt.Printf("Error loading config: %v\n", err)
					os.Exit(1)
				}
				installer := skills.NewSkillInstaller(cfg.WorkspacePath())
				skillsSearchCmd(installer)
			})
		},
	}
	skillsRoot.AddCommand(search)

	show := &cobra.Command{
		Use:     "show <skill>",
		Short:   "Show full SKILL.md content",
		Args:    cobra.ExactArgs(1),
		Example: "  dotagent skills show weather",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLegacyWithArgs([]string{"skills", "show", args[0]}, func() {
				cfg, err := loadConfig()
				if err != nil {
					fmt.Printf("Error loading config: %v\n", err)
					os.Exit(1)
				}
				workspace := cfg.WorkspacePath()
				globalDir := filepath.Dir(getConfigPath())
				globalSkillsDir := filepath.Join(globalDir, "skills")
				builtinSkillsDir := filepath.Join(globalDir, "dotagent", "skills")
				loader := skills.NewSkillsLoader(workspace, globalSkillsDir, builtinSkillsDir)
				skillsShowCmd(loader, args[0])
			})
		},
	}
	skillsRoot.AddCommand(show)

	return skillsRoot
}

func newToolpacksCommand() *cobra.Command {
	toolpacksRoot := &cobra.Command{
		Use:   "toolpacks",
		Short: "Manage executable tool packs",
		Long:  "Install, inspect, validate, and doctor executable toolpacks that extend agent capabilities.",
	}

	toolpacksRoot.AddCommand(&cobra.Command{
		Use:     "list",
		Short:   "List installed toolpacks",
		Example: "  dotagent toolpacks list",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLegacyWithArgs([]string{"toolpacks", "list"}, toolpacksCmd)
		},
	})

	install := &cobra.Command{
		Use:     "install <path|owner/repo[@ref]>",
		Short:   "Install a toolpack from local path or GitHub",
		Args:    cobra.ExactArgs(1),
		Example: "  dotagent toolpacks install ./examples/toolpacks/github-cli",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLegacyWithArgs([]string{"toolpacks", "install", args[0]}, toolpacksCmd)
		},
	}
	toolpacksRoot.AddCommand(install)

	enable := &cobra.Command{
		Use:     "enable <id>",
		Short:   "Enable a toolpack",
		Args:    cobra.ExactArgs(1),
		Example: "  dotagent toolpacks enable github-cli",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLegacyWithArgs([]string{"toolpacks", "enable", args[0]}, toolpacksCmd)
		},
	}
	toolpacksRoot.AddCommand(enable)

	disable := &cobra.Command{
		Use:     "disable <id>",
		Short:   "Disable a toolpack",
		Args:    cobra.ExactArgs(1),
		Example: "  dotagent toolpacks disable github-cli",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLegacyWithArgs([]string{"toolpacks", "disable", args[0]}, toolpacksCmd)
		},
	}
	toolpacksRoot.AddCommand(disable)

	remove := &cobra.Command{
		Use:     "remove <id>",
		Aliases: []string{"uninstall"},
		Short:   "Remove an installed toolpack",
		Args:    cobra.ExactArgs(1),
		Example: "  dotagent toolpacks remove github-cli",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLegacyWithArgs([]string{"toolpacks", "remove", args[0]}, toolpacksCmd)
		},
	}
	toolpacksRoot.AddCommand(remove)

	show := &cobra.Command{
		Use:     "show <id>",
		Short:   "Show resolved manifest metadata",
		Args:    cobra.ExactArgs(1),
		Example: "  dotagent toolpacks show github-cli",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLegacyWithArgs([]string{"toolpacks", "show", args[0]}, toolpacksCmd)
		},
	}
	toolpacksRoot.AddCommand(show)

	validate := &cobra.Command{
		Use:     "validate [id]",
		Short:   "Validate all toolpacks or one target",
		Args:    cobra.MaximumNArgs(1),
		Example: "  dotagent toolpacks validate",
		RunE: func(cmd *cobra.Command, args []string) error {
			legacyArgs := []string{"toolpacks", "validate"}
			if len(args) == 1 {
				legacyArgs = append(legacyArgs, args[0])
			}
			return runLegacyWithArgs(legacyArgs, toolpacksCmd)
		},
	}
	toolpacksRoot.AddCommand(validate)

	doctor := &cobra.Command{
		Use:     "doctor [id]",
		Short:   "Run connector health checks",
		Args:    cobra.MaximumNArgs(1),
		Example: "  dotagent toolpacks doctor github-cli",
		RunE: func(cmd *cobra.Command, args []string) error {
			legacyArgs := []string{"toolpacks", "doctor"}
			if len(args) == 1 {
				legacyArgs = append(legacyArgs, args[0])
			}
			return runLegacyWithArgs(legacyArgs, toolpacksCmd)
		},
	}
	toolpacksRoot.AddCommand(doctor)

	return toolpacksRoot
}
