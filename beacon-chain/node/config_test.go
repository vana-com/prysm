package node

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/OffchainLabs/prysm/v7/cmd"
	"github.com/OffchainLabs/prysm/v7/cmd/beacon-chain/flags"
	"github.com/OffchainLabs/prysm/v7/config/params"
	"github.com/OffchainLabs/prysm/v7/consensus-types/primitives"
	"github.com/OffchainLabs/prysm/v7/testing/assert"
	"github.com/OffchainLabs/prysm/v7/testing/require"
	"github.com/ethereum/go-ethereum/common"
	logTest "github.com/sirupsen/logrus/hooks/test"
	"github.com/urfave/cli/v2"
)

func TestConfigureHistoricalSlasher(t *testing.T) {
	params.SetupTestConfigCleanup(t)
	hook := logTest.NewGlobal()

	app := cli.App{}
	set := flag.NewFlagSet("test", 0)
	set.Bool(flags.HistoricalSlasherNode.Name, true, "")
	cliCtx := cli.NewContext(&app, set, nil)

	require.NoError(t, configureHistoricalSlasher(cliCtx))

	assert.Equal(t, params.BeaconConfig().SlotsPerEpoch*4, params.BeaconConfig().SlotsPerArchivedPoint)
	assert.LogsContain(t, hook,
		fmt.Sprintf(
			"Setting %d slots per archive point and %d max RPC page size for historical slasher usage",
			params.BeaconConfig().SlotsPerArchivedPoint,
			int(params.BeaconConfig().SlotsPerEpoch.Mul(params.BeaconConfig().MaxAttestations))),
	)
}

func TestConfigureSlotsPerArchivedPoint(t *testing.T) {
	params.SetupTestConfigCleanup(t)

	app := cli.App{}
	set := flag.NewFlagSet("test", 0)
	set.Int(flags.SlotsPerArchivedPoint.Name, 0, "")
	require.NoError(t, set.Set(flags.SlotsPerArchivedPoint.Name, strconv.Itoa(100)))
	cliCtx := cli.NewContext(&app, set, nil)

	require.NoError(t, configureSlotsPerArchivedPoint(cliCtx))

	assert.Equal(t, primitives.Slot(100), params.BeaconConfig().SlotsPerArchivedPoint)
}

func TestConfigureProofOfWork(t *testing.T) {
	params.SetupTestConfigCleanup(t)

	app := cli.App{}
	set := flag.NewFlagSet("test", 0)
	set.Uint64(flags.ChainID.Name, 0, "")
	set.Uint64(flags.NetworkID.Name, 0, "")
	set.String(flags.DepositContractFlag.Name, "", "")
	require.NoError(t, set.Set(flags.ChainID.Name, strconv.Itoa(100)))
	require.NoError(t, set.Set(flags.NetworkID.Name, strconv.Itoa(200)))
	require.NoError(t, set.Set(flags.DepositContractFlag.Name, "deposit-contract"))
	cliCtx := cli.NewContext(&app, set, nil)

	require.NoError(t, configureEth1Config(cliCtx))

	assert.Equal(t, uint64(100), params.BeaconConfig().DepositChainID)
	assert.Equal(t, uint64(200), params.BeaconConfig().DepositNetworkID)
	assert.Equal(t, "deposit-contract", params.BeaconConfig().DepositContractAddress)
}

func TestConfigureExecutionSetting(t *testing.T) {
	params.SetupTestConfigCleanup(t)
	hook := logTest.NewGlobal()

	app := cli.App{}
	set := flag.NewFlagSet("test", 0)
	set.String(flags.SuggestedFeeRecipient.Name, "", "")
	set.Uint64(flags.TerminalTotalDifficultyOverride.Name, 0, "")
	set.String(flags.TerminalBlockHashOverride.Name, "", "")
	set.Uint64(flags.TerminalBlockHashActivationEpochOverride.Name, 0, "")

	require.NoError(t, set.Set(flags.TerminalTotalDifficultyOverride.Name, strconv.Itoa(100)))
	require.NoError(t, set.Set(flags.TerminalBlockHashOverride.Name, "0xA"))
	require.NoError(t, set.Set(flags.TerminalBlockHashActivationEpochOverride.Name, strconv.Itoa(200)))
	require.NoError(t, set.Set(flags.SuggestedFeeRecipient.Name, "0xB"))
	cliCtx := cli.NewContext(&app, set, nil)
	err := configureExecutionSetting(cliCtx)
	assert.LogsContain(t, hook, "0xB is not a valid fee recipient address")
	require.NoError(t, err)

	require.NoError(t, set.Set(flags.SuggestedFeeRecipient.Name, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"))
	cliCtx = cli.NewContext(&app, set, nil)
	err = configureExecutionSetting(cliCtx)
	require.NoError(t, err)
	assert.Equal(t, common.HexToAddress("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"), params.BeaconConfig().DefaultFeeRecipient)

	assert.LogsContain(t, hook,
		"is not a checksum Ethereum address",
	)
	require.NoError(t, set.Set(flags.SuggestedFeeRecipient.Name, "0xaAaAaAaaAaAaAaaAaAAAAAAAAaaaAaAaAaaAaaAa"))
	cliCtx = cli.NewContext(&app, set, nil)
	err = configureExecutionSetting(cliCtx)
	require.NoError(t, err)
	assert.Equal(t, common.HexToAddress("0xaAaAaAaaAaAaAaaAaAAAAAAAAaaaAaAaAaaAaaAa"), params.BeaconConfig().DefaultFeeRecipient)

	assert.Equal(t, "100", params.BeaconConfig().TerminalTotalDifficulty)
	assert.Equal(t, common.HexToHash("0xA"), params.BeaconConfig().TerminalBlockHash)
	assert.Equal(t, primitives.Epoch(200), params.BeaconConfig().TerminalBlockHashActivationEpoch)

}

func TestConfigureNetwork(t *testing.T) {
	params.SetupTestConfigCleanup(t)

	app := cli.App{}
	set := flag.NewFlagSet("test", 0)
	bootstrapNodes := cli.StringSlice{}
	set.Var(&bootstrapNodes, cmd.BootstrapNode.Name, "")
	set.Int(flags.ContractDeploymentBlock.Name, 0, "")
	require.NoError(t, set.Set(cmd.BootstrapNode.Name, "node1"))
	require.NoError(t, set.Set(cmd.BootstrapNode.Name, "node2"))
	require.NoError(t, set.Set(flags.ContractDeploymentBlock.Name, strconv.Itoa(100)))
	cliCtx := cli.NewContext(&app, set, nil)

	configureNetwork(cliCtx)

	assert.DeepEqual(t, []string{"node1", "node2"}, params.BeaconNetworkConfig().BootstrapNodes)
	assert.Equal(t, uint64(100), params.BeaconNetworkConfig().ContractDeploymentBlock)
}

func TestConfigureNetwork_ConfigFile(t *testing.T) {
	app := cli.App{}
	set := flag.NewFlagSet("test", 0)
	context := cli.NewContext(&app, set, nil)

	require.NoError(t, os.WriteFile("flags_test.yaml", fmt.Appendf(nil, "%s:\n - %s\n - %s\n", cmd.BootstrapNode.Name,
		"node1",
		"node2"), 0666))

	require.NoError(t, set.Parse([]string{"test-command", "--" + cmd.ConfigFileFlag.Name, "flags_test.yaml"}))
	comFlags := cmd.WrapFlags([]cli.Flag{
		&cli.StringFlag{
			Name: cmd.ConfigFileFlag.Name,
		},
		&cli.StringSliceFlag{
			Name: cmd.BootstrapNode.Name,
		},
	})
	command := &cli.Command{
		Name:  "test-command",
		Flags: comFlags,
		Before: func(cliCtx *cli.Context) error {
			return cmd.LoadFlagsFromConfig(cliCtx, comFlags)
		},
		Action: func(cliCtx *cli.Context) error {
			require.Equal(t, true, cliCtx.IsSet(cmd.BootstrapNode.Name))

			require.Equal(t, strings.Join([]string{"node1", "node2"}, ","),
				strings.Join(cliCtx.StringSlice(cmd.BootstrapNode.Name), ","))
			return nil
		},
	}
	require.NoError(t, command.Run(context, context.Args().Slice()...))
	require.NoError(t, os.Remove("flags_test.yaml"))
}

func TestAliasFlag(t *testing.T) {
	// Create a new app with the flag
	app := &cli.App{
		Flags: []cli.Flag{flags.HTTPServerHost},
		Action: func(c *cli.Context) error {
			// Test if the alias works and sets the flag correctly
			if c.IsSet("grpc-gateway-host") && c.IsSet("http-host") {
				return nil
			}
			return cli.Exit("Alias or flag not set", 1)
		},
	}

	// Simulate command line arguments that include the alias
	args := []string{"app", "--grpc-gateway-host", "config.yml"}

	// Run the app with the simulated arguments
	err := app.Run(args)

	// Check if the alias set the flag correctly
	assert.NoError(t, err)
}

func TestValidateDepositContractSwitch(t *testing.T) {
	const current = "0x1111111111111111111111111111111111111111"
	const retired = "0x2222222222222222222222222222222222222222"

	tests := []struct {
		name        string
		retired     string
		switchBlock uint64
		deployment  uint64
		wantErr     string
	}{
		{name: "unset is allowed"},
		{name: "fully configured", retired: retired, switchBlock: 500, deployment: 100},
		{
			name:    "address without a switch block",
			retired: retired,
			wantErr: "needs both a retired contract and a switch block",
		},
		{
			name:        "switch block without an address",
			switchBlock: 500,
			wantErr:     "needs both a retired contract and a switch block",
		},
		{
			name:        "malformed address",
			retired:     "not-an-address",
			switchBlock: 500,
			wantErr:     "invalid retired deposit contract address",
		},
		{
			name:        "retired same as current",
			retired:     current,
			switchBlock: 500,
			wantErr:     "same as the current deposit contract",
		},
		{
			name:        "switch block at the deployment block",
			retired:     retired,
			switchBlock: 100,
			deployment:  100,
			wantErr:     "must be above the contract deployment block",
		},
		{
			name:        "switch block below the deployment block",
			retired:     retired,
			switchBlock: 50,
			deployment:  100,
			wantErr:     "must be above the contract deployment block",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params.SetupTestConfigCleanup(t)

			netCfg := params.BeaconNetworkConfig().Copy()
			netCfg.ContractDeploymentBlock = tt.deployment
			params.OverrideBeaconNetworkConfig(netCfg)

			c := params.BeaconConfig().Copy()
			c.DepositContractAddress = current
			c.RetiredDepositContractAddress = tt.retired
			c.DepositContractSwitchBlock = tt.switchBlock

			err := validateDepositContractSwitch(c)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.NotNil(t, err)
			assert.StringContains(t, tt.wantErr, err.Error())
		})
	}
}

func TestConfigureEth1Config_DepositContractSwitchFlags(t *testing.T) {
	params.SetupTestConfigCleanup(t)

	netCfg := params.BeaconNetworkConfig().Copy()
	netCfg.ContractDeploymentBlock = 100
	params.OverrideBeaconNetworkConfig(netCfg)

	const retired = "0x2222222222222222222222222222222222222222"

	app := cli.App{}
	set := flag.NewFlagSet("test", 0)
	set.String(flags.DepositContractFlag.Name, "", "")
	set.String(flags.RetiredDepositContract.Name, "", "")
	set.Uint64(flags.DepositContractSwitchBlock.Name, 0, "")
	require.NoError(t, set.Set(flags.DepositContractFlag.Name, "0x1111111111111111111111111111111111111111"))
	require.NoError(t, set.Set(flags.RetiredDepositContract.Name, retired))
	require.NoError(t, set.Set(flags.DepositContractSwitchBlock.Name, "500"))

	require.NoError(t, configureEth1Config(cli.NewContext(&app, set, nil)))
	assert.Equal(t, retired, params.BeaconConfig().RetiredDepositContractAddress)
	assert.Equal(t, uint64(500), params.BeaconConfig().DepositContractSwitchBlock)
}

// TestConfigureBeacon_DepositContractSwitchUsesOverriddenDeploymentBlock exercises the real startup
// order. The switch is validated against ContractDeploymentBlock, which configureNetwork overrides
// from a flag, so validating before that override compares against a value the node never uses.
// TestValidateDepositContractSwitch cannot catch this: it sets the network config directly.
func TestConfigureBeacon_DepositContractSwitchUsesOverriddenDeploymentBlock(t *testing.T) {
	const (
		current = "0x1111111111111111111111111111111111111111"
		retired = "0x2222222222222222222222222222222222222222"
	)

	newCtx := func(deploymentDefault uint64, deploymentFlag int, switchBlock uint64) *cli.Context {
		netCfg := params.BeaconNetworkConfig().Copy()
		netCfg.ContractDeploymentBlock = deploymentDefault
		params.OverrideBeaconNetworkConfig(netCfg)

		set := flag.NewFlagSet("test", 0)
		set.String(flags.DepositContractFlag.Name, "", "")
		set.String(flags.RetiredDepositContract.Name, "", "")
		set.Uint64(flags.DepositContractSwitchBlock.Name, 0, "")
		set.Int(flags.ContractDeploymentBlock.Name, 0, "")
		require.NoError(t, set.Set(flags.DepositContractFlag.Name, current))
		require.NoError(t, set.Set(flags.RetiredDepositContract.Name, retired))
		require.NoError(t, set.Set(flags.DepositContractSwitchBlock.Name, strconv.FormatUint(switchBlock, 10)))
		require.NoError(t, set.Set(flags.ContractDeploymentBlock.Name, strconv.Itoa(deploymentFlag)))
		return cli.NewContext(&cli.App{}, set, nil)
	}

	t.Run("accepts a switch above the overridden deployment block", func(t *testing.T) {
		params.SetupTestConfigCleanup(t)
		// The flag lowers the deployment block to 10, so a switch at 500 is valid. Validating before
		// the override would compare 500 against the 1,000,000 default and reject it.
		require.NoError(t, configureBeacon(newCtx(1_000_000, 10, 500)))
		assert.Equal(t, uint64(10), params.BeaconNetworkConfig().ContractDeploymentBlock)
		assert.Equal(t, uint64(500), params.BeaconConfig().DepositContractSwitchBlock)
	})

	t.Run("rejects a switch at or below the overridden deployment block", func(t *testing.T) {
		params.SetupTestConfigCleanup(t)
		// The flag raises the deployment block to 900, above the switch at 500, so the retired
		// contract would never be scanned. Validating before the override would compare 500 against
		// the 0 default and let this through.
		err := configureBeacon(newCtx(0, 900, 500))
		require.NotNil(t, err, "a switch block below the effective deployment block must be rejected")
		assert.StringContains(t, "must be above the contract deployment block", err.Error())
	})
}
