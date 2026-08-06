// andino-kb is a headless, self-hosted RAG server: declarative indexing
// pipelines over your documents, hybrid search, and an MCP + REST API for
// coding agents and autonomous agents.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/andino-agents/knowledge-base/internal/config"
)

// version is set by the linker at release time (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	var configPath string

	root := &cobra.Command{
		Use:           "andino-kb",
		Short:         "Self-hosted knowledge bases for AI agents (hybrid RAG over MCP and REST)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "config.yaml", "path to config file")

	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the andino-kb version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("andino-kb", version)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "Load and validate the configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			fmt.Printf("OK: %d knowledge base(s), storage provider %q\n",
				len(cfg.KnowledgeBases), cfg.Storage.Provider)
			return nil
		},
	})

	root.AddCommand(indexCmd(&configPath))
	root.AddCommand(searchCmd(&configPath))
	root.AddCommand(serveCmd(&configPath))

	// parity lands in phase 5.
	return root
}
