package utils

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/kwilteam/kwil-db/app/shared/display"
	"github.com/kwilteam/kwil-db/cmd/kwil-cli/client"
	"github.com/kwilteam/kwil-db/cmd/kwil-cli/config"
	clientType "github.com/kwilteam/kwil-db/core/client/types"
)

var (
	chainInfoLong = `Display information about the connected Kwil network.`
)

func chainInfoCmd() *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "chain-info",
		Short: chainInfoLong,
		Long:  chainInfoLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return client.DialClient(cmd.Context(), cmd, client.WithoutPrivateKey, func(ctx context.Context, client clientType.Client, cfg *config.KwilCliConfig) error {
				chainInfo, err := client.ChainInfo(ctx)
				if err != nil {
					return display.PrintErr(cmd, err)
				}

				return display.PrintCmd(cmd, &respChainInfo{Info: chainInfo})
			})
		},
	}

	return cmd
}
