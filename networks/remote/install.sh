#!/usr/bin/env bash

NUM_NODES=5
TESTNET_NAME="sentrynet"
SSH_KEY_FILE="path to ssh key"
DO_API_TOKEN="DO API token"


# Generate configs for nodes
rm -rf $GOPATH/src/github.com/tendermint/tendermint/networks/remote/nodes/list/*
go run $GOPATH/src/github.com/tendermint/tendermint/networks/remote/nodes/node.go -N=$NUM_NODES


cd $GOPATH/src/github.com/tendermint/tendermint/networks/remote/terraform
terraform init
terraform apply -var DO_API_TOKEN="$DO_API_TOKEN" -var SSH_KEY_FILE="$SSH_KEY_FILE" -var TESTNET_NAME="$TESTNET_NAME" -var SERVERS="$NUM_NODES" -auto-approve

# let the droplets boot
sleep 120


# all the ansible commands are also directory specific
cd $GOPATH/src/github.com/tendermint/tendermint/networks/remote/ansible

ansible-playbook -i inventory/digital_ocean.py -l $TESTNET_NAME install.yml
ansible-playbook -i inventory/digital_ocean.py -l $TESTNET_NAME config.yml -e BINARY=$GOPATH/src/github.com/tendermint/tendermint/build/tendermint -e CONFIGDIR=$GOPATH/src/github.com/tendermint/tendermint/networks/remote/nodes/list -e "{\"N\":$NUM_NODES}"


sleep 30
#Update ansble. Add persistent node connections
go run $GOPATH/src/github.com/tendermint/tendermint/networks/remote/nodes/update_template.go

# now, we can re-run the install command
ansible-playbook -i inventory/digital_ocean.py -l $TESTNET_NAME install.yml

# and finally restart it all
ansible-playbook -i inventory/digital_ocean.py -l $TESTNET_NAME restart.yml
# install node exporter
ansible-playbook -i inventory/digital_ocean.py -l $TESTNET_NAME node_exporter.yml

echo "congratulations, your testnet is now running :)"
