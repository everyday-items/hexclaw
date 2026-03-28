package main

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/skill/hub"
)

func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Manage MCP servers",
	}

	cmd.AddCommand(
		newMCPListCmd(),
		newMCPSearchCmd(),
		newMCPInstallCmd(),
		newMCPRemoveCmd(),
	)
	return cmd
}

func newMCPListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed MCP servers",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load("")
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			if len(cfg.MCP.Servers) == 0 {
				fmt.Println("No MCP servers configured.")
				return nil
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tTRANSPORT\tENABLED")
			for _, s := range cfg.MCP.Servers {
				fmt.Fprintf(tw, "%s\t%s\t%v\n", s.Name, s.Transport, s.Enabled)
			}
			return tw.Flush()
		},
	}
}

func newMCPSearchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "search [keyword]",
		Short: "Search MCP servers in HexClaw Hub",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			h := hub.NewMcpHub("")
			if err := h.Refresh(); err != nil {
				return fmt.Errorf("refresh hub: %w", err)
			}

			query := ""
			if len(args) > 0 {
				query = args[0]
			}
			results := h.Search(query)

			if len(results) == 0 {
				fmt.Println("No MCP servers found.")
				return nil
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tDESCRIPTION\tCATEGORY")
			for _, s := range results {
				desc := s.Description
				if len(desc) > 50 {
					desc = desc[:47] + "..."
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Name, desc, s.Category)
			}
			return tw.Flush()
		},
	}
}

func newMCPInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install <name>",
		Short: "Install MCP server from HexClaw Hub",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			h := hub.NewMcpHub("")
			if err := h.Refresh(); err != nil {
				return fmt.Errorf("refresh hub: %w", err)
			}

			meta, err := h.Get(name)
			if err != nil {
				return err
			}

			home, _ := os.UserHomeDir()
			cfgPath := filepath.Join(home, ".hexclaw", "hexclaw.yaml")
			w := config.NewWriter(cfgPath)

			if err := w.AppendMCPServer(meta.Name, "stdio", meta.Command, meta.Args, ""); err != nil {
				return err
			}

			fmt.Printf("Installed MCP server '%s' (%s)\n", meta.Name, meta.Description)
			fmt.Println("Restart hexclaw to activate, or use the API to add at runtime.")
			return nil
		},
	}
}

func newMCPRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove MCP server from config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			home, _ := os.UserHomeDir()
			cfgPath := filepath.Join(home, ".hexclaw", "hexclaw.yaml")
			w := config.NewWriter(cfgPath)

			if err := w.RemoveMCPServer(name); err != nil {
				return err
			}

			fmt.Printf("Removed MCP server '%s'\n", name)
			return nil
		},
	}
}
