package rpc

import (
	"context"

	"github.com/kwilteam/kwil-db/app/shared/display"
	"github.com/spf13/cobra"
)

var (
	versionLong = `Print the node's version information. The version is the Kwil's version string, set at compile time.`

	versionExample = `# Print the node's version information
kwil-admin node version --rpcserver /tmp/kwild.socket`
)

func versionCmd() *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "version",
		Short:   "Print the node's version information.",
		Long:    versionLong,
		Example: versionExample,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := context.Background()
			client, err := AdminSvcClient(ctx, cmd)
			if err != nil {
				return display.PrintErr(cmd, err)
			}

			version, err := client.Version(ctx)
			if err != nil {
				return display.PrintErr(cmd, err)
			}

			return display.PrintCmd(cmd, display.RespString(version))
		},
	}

	BindRPCFlags(cmd)

	return cmd
}
