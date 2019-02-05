package client_test

import (
	"os"
	"testing"

	"github.com/tendermint/tendermint/types"

	"github.com/tendermint/tendermint/abci/example/kvstore"
	nm "github.com/tendermint/tendermint/node"
	rpctest "github.com/tendermint/tendermint/rpc/test"
)

var node *nm.Node

func TestMain(m *testing.M) {
	// start a tendermint node (and kvstore) in the background to test against
	app := kvstore.NewKVStoreApplication()
	node = rpctest.StartTendermint(app)
	node.ConsensusState().SetVerifier(&types.MockVerifier{})
	code := m.Run()

	// and shut down proper at the end
	node.Stop()
	node.Wait()
	os.Exit(code)
}
