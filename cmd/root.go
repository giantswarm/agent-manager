// Package cmd holds the agent-manager CLI.
package cmd

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var (
	version     = "dev"
	buildCommit = "unknown"
	buildDate   = "unknown"
)

// SetVersion records the build version (set from main via ldflags).
func SetVersion(v string) { version = v }

// SetBuildInfo records the commit and build date.
func SetBuildInfo(commit, date string) {
	buildCommit = commit
	buildDate = date
}

func newRootCmd() *cobra.Command {
	var verbose bool
	root := &cobra.Command{
		Use:   "agent-manager",
		Short: "Agent lifecycle service for the Agent Platform",
		Long: `agent-manager is the write surface for agents on the Agent Platform: it
creates, updates, deletes and inspects kagent agents as Flux HelmReleases of
the agent chart (one release = one Agent), validates the values against the
chart's schema before anything is applied, and lists the ModelConfigs and
skills an agent can be built from. The API is served as REST/JSON (portal) and
as MCP tools (muster) from one process.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			level := slog.LevelInfo
			if verbose {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
		},
	}
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable debug logging")
	root.Version = version
	root.SetVersionTemplate("agent-manager version {{.Version}}\n")
	root.AddCommand(newServeCmd(), newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "agent-manager version %s\n  commit: %s\n  built:  %s\n", version, buildCommit, buildDate)
		},
	}
}

// Execute runs the CLI.
func Execute() {
	if err := newRootCmd().Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
