package cli

import (
	"ec2cp/src/config"
	"ec2cp/src/server"

	"github.com/spf13/cobra"
)

func serveCmd() *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the HTTP server + web UI",
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := config.LoadEnv()
			if err != nil {
				return err
			}
			return server.Run(cmd.Context(), env, port)
		},
	}
	cmd.Flags().IntVar(&port, "port", 2721, "listen port")
	return cmd
}
