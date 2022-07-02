package chainlink

import (
	"fmt"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"go.uber.org/multierr"

	solcfg "github.com/smartcontractkit/chainlink-solana/pkg/solana/config"
	soldb "github.com/smartcontractkit/chainlink-solana/pkg/solana/db"
	tercfg "github.com/smartcontractkit/chainlink-terra/pkg/terra/config"
	terdb "github.com/smartcontractkit/chainlink-terra/pkg/terra/db"
	evmcfg "github.com/smartcontractkit/chainlink/core/chains/evm/config/v2"
	evmtyp "github.com/smartcontractkit/chainlink/core/chains/evm/types"
	"github.com/smartcontractkit/chainlink/core/chains/solana"
	tertyp "github.com/smartcontractkit/chainlink/core/chains/terra/types"
	coreconfig "github.com/smartcontractkit/chainlink/core/config"
	config "github.com/smartcontractkit/chainlink/core/config/v2"
	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/utils"
)

// Config is the root type used for TOML configuration.
//
// See docs at /docs/CONFIG.md generated via config.GenerateDocs from /internal/config/docs.toml
//
// When adding a new field:
// 	- consider including a unit suffix with the field name
// 	- TOML is limited to int64/float64, so fields requiring greater range/precision must use non-standard types
//	implementing encoding.TextMarshaler/TextUnmarshaler, like utils.Big and decimal.Decimal
//  - std lib types that don't implement encoding.TextMarshaler/TextUnmarshaler (time.Duration, url.URL, big.Int) won't
//   work as expected, and require wrapper types. See models.Duration, models.URL, utils.Big.
type Config struct {
	config.Core

	EVM EVMConfigs `toml:",omitempty"`

	Solana SolanaConfigs `toml:",omitempty"`

	Terra TerraConfigs `toml:",omitempty"`
}

func NewConfig(tomlString string, lggr logger.Logger) (coreconfig.GeneralConfig, error) {
	lggr = lggr.Named("Config")
	var c Config
	err := toml.Unmarshal([]byte(tomlString), &c)
	if err != nil {
		//TODO check for strict error to unroll friendlier String() format; lock in with test
		return nil, err
	}
	input, err := c.TOMLString()
	if err != nil {
		return nil, err
	}
	//TODO drop if diff is good enough
	lggr.Info("Input Configuration", "config", input)

	//TODO c.SetDefaults()

	effective, err := c.TOMLString()
	if err != nil {
		return nil, err
	}
	//TODO can we comment the defaults somehow? or maybe use diff.Diff ?
	lggr.Info("Effective Configuration, with defaults applied", "config", effective)
	return &legacyGeneralConfig{c: &c, lggr: lggr}, nil
}

// TOMLString returns a pretty-printed TOML encoded string, with extra line breaks removed.
func (c *Config) TOMLString() (string, error) {
	b, err := toml.Marshal(c)
	if err != nil {
		return "", err
	}
	// remove runs of line breaks
	s := multiLineBreak.ReplaceAllLiteralString(string(b), "\n")
	// restore them preceding keys
	s = strings.Replace(s, "\n[", "\n\n[", -1)
	s = strings.TrimPrefix(s, "\n")
	return s, nil
}

func (c *Config) Validate() error {
	err := c.Core.ValidateConfig()
	err = config.Validate(err, c.EVM, "EVM")
	err = config.Validate(err, c.Solana, "Solana")
	err = config.Validate(err, c.Terra, "Terra")
	return utils.MultiErrorList(err)
}

type EVMConfigs []*EVMConfig

func (cs EVMConfigs) ValidateConfig() (err error) {
	chainIDs := map[string]struct{}{}
	for _, c := range cs {
		chainID := c.ChainID.String()
		//TODO non-empty chain id
		if _, ok := chainIDs[chainID]; ok {
			err = multierr.Append(err, fmt.Errorf("duplicate chain id: %s", chainID))
		} else {
			chainIDs[chainID] = struct{}{}
		}
		err = multierr.Append(err, c.ValidateConfig())
	}
	//TODO at least one node?
	//TODO
	return
}

type EVMNodes []*evmcfg.Node

func (ns EVMNodes) ValidateConfig() (err error) {
	names := map[string]struct{}{}
	for _, n := range ns {
		//TODO non-empty name
		if _, ok := names[n.Name]; ok {
			err = multierr.Append(err, fmt.Errorf("duplicate node name: %s", n.Name))
		}
		names[n.Name] = struct{}{}
		err = multierr.Append(err, n.ValidateConfig())
	}
	//TODO
	return
}

type EVMConfig struct {
	ChainID *utils.Big
	Enabled *bool
	evmcfg.Chain
	Nodes EVMNodes
}

func (c *EVMConfig) ValidateConfig() (err error) {
	err = config.Validate(err, &c.Chain, "Chain")
	err = config.Validate(err, c.Nodes, "Nodes")
	//TODO
	return
}

func (c *EVMConfig) setFromDB(ch evmtyp.DBChain, nodes []evmtyp.Node) error {
	c.ChainID = &ch.ID
	c.Enabled = &ch.Enabled

	if err := c.Chain.SetFromDB(ch.Cfg); err != nil {
		return err
	}
	for _, db := range nodes {
		var n evmcfg.Node
		if err := n.SetFromDB(db); err != nil {
			return err
		}
		c.Nodes = append(c.Nodes, &n)
	}
	return nil
}

type SolanaConfigs []*SolanaConfig

func (cs SolanaConfigs) ValidateConfig() (err error) {
	chainIDs := map[string]struct{}{}
	for _, c := range cs {
		//TODO non-empty chain id
		if _, ok := chainIDs[c.ChainID]; ok {
			err = multierr.Append(err, fmt.Errorf("duplicate chain id: %s", c.ChainID))
		} else {
			chainIDs[c.ChainID] = struct{}{}
		}
		err = multierr.Append(err, c.ValidateConfig())
	}
	//TODO at least one node?
	//TODO
	return
}

type SolanaNodes []*solcfg.Node

func (ns SolanaNodes) ValidateConfig() (err error) {
	names := map[string]struct{}{}
	for _, n := range ns {
		//TODO non-empty name
		if _, ok := names[n.Name]; ok {
			err = multierr.Append(err, fmt.Errorf("duplicate node name: %s", n.Name))
		}
		names[n.Name] = struct{}{}
		//TODO err = multierr.Append(err, n.ValidateConfig())
	}
	//TODO
	return
}

type SolanaConfig struct {
	ChainID string
	Enabled *bool
	solcfg.Chain
	Nodes SolanaNodes
}

func (c *SolanaConfig) ValidateConfig() (err error) {
	//TODO err = config.Validate(err, &c.Chain, "Chain")
	err = config.Validate(err, c.Nodes, "Nodes")
	//TODO
	return
}

func (c *SolanaConfig) setFromDB(ch solana.DBChain, nodes []soldb.Node) error {
	c.ChainID = ch.ID
	c.Enabled = &ch.Enabled

	if err := c.Chain.SetFromDB(ch.Cfg); err != nil {
		return err
	}
	for _, db := range nodes {
		var n solcfg.Node
		if err := n.SetFromDB(db); err != nil {
			return err
		}
		c.Nodes = append(c.Nodes, &n)
	}
	return nil
}

type TerraConfigs []*TerraConfig

func (cs TerraConfigs) ValidateConfig() (err error) {
	chainIDs := map[string]struct{}{}
	for _, c := range cs {
		//TODO non-empty chain id
		if _, ok := chainIDs[c.ChainID]; ok {
			err = multierr.Append(err, fmt.Errorf("duplicate chain id: %s", c.ChainID))
		} else {
			chainIDs[c.ChainID] = struct{}{}
		}
		err = multierr.Append(err, c.ValidateConfig())
	}
	//TODO at least one node?
	//TODO
	return
}

type TerraNodes []*tercfg.Node

func (ns TerraNodes) ValidateConfig() (err error) {
	names := map[string]struct{}{}
	for _, n := range ns {
		//TODO non-empty name
		if _, ok := names[n.Name]; ok {
			err = multierr.Append(err, fmt.Errorf("duplicate node name: %s", n.Name))
		}
		names[n.Name] = struct{}{}
		//TODO err = multierr.Append(err, n.ValidateConfig())
	}
	//TODO
	return
}

type TerraConfig struct {
	ChainID string
	Enabled *bool
	tercfg.Chain
	Nodes TerraNodes
}

func (c *TerraConfig) ValidateConfig() (err error) {
	//TODO err = config.Validate(err, &c.Chain, "Chain")
	err = config.Validate(err, c.Nodes, "Nodes")
	//TODO
	return
}

func (c *TerraConfig) setFromDB(ch tertyp.DBChain, nodes []terdb.Node) error {
	c.ChainID = ch.ID
	c.Enabled = &ch.Enabled

	if err := c.Chain.SetFromDB(ch.Cfg); err != nil {
		return err
	}
	for _, db := range nodes {
		var n tercfg.Node
		if err := n.SetFromDB(db); err != nil {
			return err
		}
		c.Nodes = append(c.Nodes, &n)
	}
	return nil
}
