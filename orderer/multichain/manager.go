/*
Copyright IBM Corp. 2016 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

                 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package multichain

import (
	"fmt"

	"github.com/hyperledger/fabric/common/cauthdsl"
	"github.com/hyperledger/fabric/common/configtx"
	"github.com/hyperledger/fabric/common/policies"
	"github.com/hyperledger/fabric/orderer/common/sharedconfig"
	"github.com/hyperledger/fabric/orderer/rawledger"
	cb "github.com/hyperledger/fabric/protos/common"
	"github.com/hyperledger/fabric/protos/utils"
	"github.com/op/go-logging"

	"github.com/golang/protobuf/proto"
)

var logger = logging.MustGetLogger("orderer/multichain")

// XXX This crypto helper is a stand in until we have a real crypto handler
// it considers all signatures to be valid
type xxxCryptoHelper struct{}

func (xxx xxxCryptoHelper) VerifySignature(sd *cb.SignedData) error {
	return nil
}

func (xxx xxxCryptoHelper) NewSignatureHeader() *cb.SignatureHeader {
	return &cb.SignatureHeader{}
}

func (xxx xxxCryptoHelper) Sign(message []byte) []byte {
	return message
}

// Signer is a temporary stub interface which will be implemented by the local MSP
type Signer interface {
	// NewSignatureHeader creates a SignatureHeader with the correct signing identity and a valid nonce
	NewSignatureHeader() *cb.SignatureHeader

	// Sign a message which should embed a signature header created by NewSignatureHeader
	Sign(message []byte) []byte
}

// Manager coordinates the creation and access of chains
type Manager interface {
	// GetChain retrieves the chain support for a chain (and whether it exists)
	GetChain(chainID string) (ChainSupport, bool)

	// ProposeChain accepts a configuration transaction for a chain which does not already exists
	// The status returned is whether the proposal is accepted for consideration, only after consensus
	// occurs will the proposal be committed or rejected
	ProposeChain(env *cb.Envelope) cb.Status
}

type multiLedger struct {
	chains        map[string]*chainSupport
	consenters    map[string]Consenter
	ledgerFactory rawledger.Factory
	sysChain      *systemChain
}

func getConfigTx(reader rawledger.Reader) *cb.Envelope {
	lastBlock := rawledger.GetBlock(reader, reader.Height()-1)
	index, err := utils.GetLastConfigurationIndexFromBlock(lastBlock)
	if err != nil {
		logger.Panicf("Chain did not have appropriately encoded last configuration in its latest block: %s", err)
	}
	configBlock := rawledger.GetBlock(reader, index)
	if configBlock == nil {
		logger.Panicf("Configuration block does not exist")
	}

	return utils.ExtractEnvelopeOrPanic(configBlock, 0)
}

// NewManagerImpl produces an instance of a Manager
func NewManagerImpl(ledgerFactory rawledger.Factory, consenters map[string]Consenter) Manager {
	ml := &multiLedger{
		chains:        make(map[string]*chainSupport),
		ledgerFactory: ledgerFactory,
		consenters:    consenters,
	}

	existingChains := ledgerFactory.ChainIDs()
	for _, chainID := range existingChains {
		rl, err := ledgerFactory.GetOrCreate(chainID)
		if err != nil {
			logger.Fatalf("Ledger factory reported chainID %s but could not retrieve it: %s", chainID, err)
		}
		configTx := getConfigTx(rl)
		if configTx == nil {
			logger.Fatalf("Could not find configuration transaction for chain %s", chainID)
		}
		configManager, policyManager, backingLedger, sharedConfigManager := ml.newResources(configTx)
		chainID := configManager.ChainID()

		if sharedConfigManager.ChainCreators() != nil {
			if ml.sysChain != nil {
				logger.Fatalf("There appear to be two system chains %s and %s", ml.sysChain.support.ChainID(), chainID)
			}
			logger.Debugf("Starting with system chain: %x", chainID)
			chain := newChainSupport(createSystemChainFilters(ml, configManager, policyManager, sharedConfigManager),
				configManager,
				policyManager,
				backingLedger,
				sharedConfigManager,
				consenters,
				&xxxCryptoHelper{})
			ml.chains[string(chainID)] = chain
			ml.sysChain = newSystemChain(chain)
			// We delay starting this chain, as it might try to copy and replace the chains map via newChain before the map is fully built
			defer chain.start()
		} else {
			logger.Debugf("Starting chain: %x", chainID)
			chain := newChainSupport(createStandardFilters(configManager, policyManager, sharedConfigManager),
				configManager,
				policyManager,
				backingLedger,
				sharedConfigManager,
				consenters,
				&xxxCryptoHelper{})
			ml.chains[string(chainID)] = chain
			chain.start()
		}

	}

	return ml
}

// ProposeChain accepts a configuration transaction for a chain which does not already exists
// The status returned is whether the proposal is accepted for consideration, only after consensus
// occurs will the proposal be committed or rejected
func (ml *multiLedger) ProposeChain(env *cb.Envelope) cb.Status {
	return ml.sysChain.proposeChain(env)
}

// GetChain retrieves the chain support for a chain (and whether it exists)
func (ml *multiLedger) GetChain(chainID string) (ChainSupport, bool) {
	cs, ok := ml.chains[chainID]
	return cs, ok
}

func newConfigTxManagerAndHandlers(configEnvelope *cb.ConfigurationEnvelope) (configtx.Manager, policies.Manager, sharedconfig.Manager, error) {
	policyProviderMap := make(map[int32]policies.Provider)
	for pType := range cb.Policy_PolicyType_name {
		rtype := cb.Policy_PolicyType(pType)
		switch rtype {
		case cb.Policy_UNKNOWN:
			// Do not register a handler
		case cb.Policy_SIGNATURE:
			policyProviderMap[pType] = cauthdsl.NewPolicyProvider(xxxCryptoHelper{})
		case cb.Policy_MSP:
			// Add hook for MSP Handler here
		}
	}
	policyManager := policies.NewManagerImpl(policyProviderMap)
	sharedConfigManager := sharedconfig.NewManagerImpl()
	configHandlerMap := make(map[cb.ConfigurationItem_ConfigurationType]configtx.Handler)
	for ctype := range cb.ConfigurationItem_ConfigurationType_name {
		rtype := cb.ConfigurationItem_ConfigurationType(ctype)
		switch rtype {
		case cb.ConfigurationItem_Policy:
			configHandlerMap[rtype] = policyManager
		case cb.ConfigurationItem_Orderer:
			configHandlerMap[rtype] = sharedConfigManager
		default:
			configHandlerMap[rtype] = configtx.NewBytesHandler()
		}
	}

	configManager, err := configtx.NewConfigurationManager(configEnvelope, policyManager, configHandlerMap)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("Error unpacking configuration transaction: %s", err)
	}

	return configManager, policyManager, sharedConfigManager, nil
}

func (ml *multiLedger) newResources(configTx *cb.Envelope) (configtx.Manager, policies.Manager, rawledger.ReadWriter, sharedconfig.Manager) {
	payload := &cb.Payload{}
	err := proto.Unmarshal(configTx.Payload, payload)
	if err != nil {
		logger.Fatalf("Error unmarshaling a config transaction payload: %s", err)
	}

	configEnvelope := &cb.ConfigurationEnvelope{}
	err = proto.Unmarshal(payload.Data, configEnvelope)
	if err != nil {
		logger.Fatalf("Error unmarshaling a config transaction to config envelope: %s", err)
	}

	configManager, policyManager, sharedConfigManager, err := newConfigTxManagerAndHandlers(configEnvelope)

	if err != nil {
		logger.Fatalf("Error creating configtx manager and handlers: %s", err)
	}

	chainID := configManager.ChainID()

	ledger, err := ml.ledgerFactory.GetOrCreate(chainID)
	if err != nil {
		logger.Fatalf("Error getting ledger for %s", chainID)
	}

	return configManager, policyManager, ledger, sharedConfigManager
}

func (ml *multiLedger) systemChain() *systemChain {
	return ml.sysChain
}

func (ml *multiLedger) newChain(configtx *cb.Envelope) {
	configManager, policyManager, backingLedger, sharedConfig := ml.newResources(configtx)
	backingLedger.Append(rawledger.CreateNextBlock(backingLedger, []*cb.Envelope{configtx}))

	// Copy the map to allow concurrent reads from broadcast/deliver while the new chainSupport is
	newChains := make(map[string]*chainSupport)
	for key, value := range ml.chains {
		newChains[key] = value
	}

	cs := newChainSupport(createStandardFilters(configManager, policyManager, sharedConfig), configManager, policyManager, backingLedger, sharedConfig, ml.consenters, &xxxCryptoHelper{})
	chainID := configManager.ChainID()

	logger.Debugf("Created and starting new chain %s", chainID)

	newChains[string(chainID)] = cs
	cs.start()

	ml.chains = newChains
}
