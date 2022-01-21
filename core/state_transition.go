// (c) 2019-2020, Ava Labs, Inc.
//
// This file is a derived work, based on the go-ethereum library whose original
// notices appear below.
//
// It is distributed under a license compatible with the licensing terms of the
// original code from which it is derived.
//
// Much love to the original authors for their work.
// **********
// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"

	"github.com/flare-foundation/coreth/core/types"
	"github.com/flare-foundation/coreth/core/vm"
	"github.com/flare-foundation/coreth/params"
)

var (
	emptyCodeHash = crypto.Keccak256Hash(nil)

	flareChainID    = new(big.Int).SetUint64(14) // https://github.com/ethereum-lists/chains/blob/master/_data/chains/eip155-14.json
	songbirdChainID = new(big.Int).SetUint64(19) // https://github.com/ethereum-lists/chains/blob/master/_data/chains/eip155-19.json

	flareStateConnectorActivationTime    = new(big.Int).SetUint64(1000000000000)
	songbirdStateConnectorActivationTime = new(big.Int).SetUint64(1000000000000)

	songbirdStateConnectorAddress = common.HexToAddress("0x6b5DEa84F71052c1302b5fe652e17FD442D126a9")
	defaultStateConnectorAddress  = common.HexToAddress("0x1000000000000000000000000000000000000001")
)

/*
StateTransition represents The State Transitioning Model

A state transition is a change made when a transaction is applied to the current world state
The state transitioning model does all the necessary work to work out a valid new state root.

1) Nonce handling
2) Pre pay gas
3) Create a new state object if the recipient is \0*32
4) Value transfer
== If contract creation ==
  4a) Attempt to run transaction data
  4b) If valid, use result as code for the new state object
== end ==
5) Run Script section
6) Derive new state root
*/
type StateTransition struct {
	gp             *GasPool
	msg            Message
	gas            uint64
	gasPrice       *big.Int
	gasFeeCap      *big.Int
	gasTipCap      *big.Int
	initialGas     uint64
	value          *big.Int
	data           []byte
	state          vm.StateDB
	evm            *vm.EVM
	stateConnector *stateConnector
}

// NewStateTransition initialises and returns a new state transition object.
func NewStateTransition(evm *vm.EVM, msg Message, gp *GasPool) *StateTransition {
	return &StateTransition{
		gp:             gp,
		evm:            evm,
		msg:            msg,
		gasPrice:       msg.GasPrice(),
		gasFeeCap:      msg.GasFeeCap(),
		gasTipCap:      msg.GasTipCap(),
		value:          msg.Value(),
		data:           msg.Data(),
		state:          evm.StateDB,
		stateConnector: newConnector(newStateConnectorCaller(evm), msg),
	}
}

// Message represents a message sent to a contract.
type Message interface {
	From() common.Address
	To() *common.Address

	GasPrice() *big.Int
	GasFeeCap() *big.Int
	GasTipCap() *big.Int
	Gas() uint64
	Value() *big.Int

	Nonce() uint64
	IsFake() bool
	Data() []byte
	AccessList() types.AccessList
}

// ExecutionResult includes all output after executing given evm
// message no matter the execution itself is successful or not.
type ExecutionResult struct {
	UsedGas    uint64 // Total used gas but include the refunded gas
	Err        error  // Any error encountered during the execution(listed in core/vm/errors.go)
	ReturnData []byte // Returned data from evm(function result or data supplied with revert opcode)
}

// Unwrap returns the internal evm error which allows us for further
// analysis outside.
func (result *ExecutionResult) Unwrap() error {
	return result.Err
}

// Failed returns the indicator whether the execution is successful or not
func (result *ExecutionResult) Failed() bool { return result.Err != nil }

// Return is a helper function to help caller distinguish between revert reason
// and function return. Return returns the data after execution if no error occurs.
func (result *ExecutionResult) Return() []byte {
	if result.Err != nil {
		return nil
	}
	return common.CopyBytes(result.ReturnData)
}

// Implement the EVMCaller interface on the state transition structure; simply delegate the calls
func (st *StateTransition) Call(caller vm.ContractRef, addr common.Address, input []byte, gas uint64, value *big.Int) (ret []byte, leftOverGas uint64, err error) {
	return st.evm.Call(caller, addr, input, gas, value)
}

func (st *StateTransition) GetBlockNumber() *big.Int {
	return st.evm.Context.BlockNumber
}

func (st *StateTransition) GetGasLimit() uint64 {
	return st.evm.Context.GasLimit
}

func (st *StateTransition) AddBalance(addr common.Address, amount *big.Int) {
	st.state.AddBalance(addr, amount)
}

// Revert returns the concrete revert reason if the execution is aborted by `REVERT`
// opcode. Note the reason can be nil if no data supplied with revert opcode.
func (result *ExecutionResult) Revert() []byte {
	if result.Err != vm.ErrExecutionReverted {
		return nil
	}
	return common.CopyBytes(result.ReturnData)
}

// IntrinsicGas computes the 'intrinsic gas' for a message with the given data.
func IntrinsicGas(data []byte, accessList types.AccessList, isContractCreation bool, isHomestead, isEIP2028 bool) (uint64, error) {
	// Set the starting gas for the raw transaction
	var gas uint64
	if isContractCreation && isHomestead {
		gas = params.TxGasContractCreation
	} else {
		gas = params.TxGas
	}
	// Bump the required gas by the amount of transactional data
	if len(data) > 0 {
		// Zero and non-zero bytes are priced differently
		var nz uint64
		for _, byt := range data {
			if byt != 0 {
				nz++
			}
		}
		// Make sure we don't exceed uint64 for all data combinations
		nonZeroGas := params.TxDataNonZeroGasFrontier
		if isEIP2028 {
			nonZeroGas = params.TxDataNonZeroGasEIP2028
		}
		if (math.MaxUint64-gas)/nonZeroGas < nz {
			return 0, ErrGasUintOverflow
		}
		gas += nz * nonZeroGas

		z := uint64(len(data)) - nz
		if (math.MaxUint64-gas)/params.TxDataZeroGas < z {
			return 0, ErrGasUintOverflow
		}
		gas += z * params.TxDataZeroGas
	}
	if accessList != nil {
		gas += uint64(len(accessList)) * params.TxAccessListAddressGas
		gas += uint64(accessList.StorageKeys()) * params.TxAccessListStorageKeyGas
	}
	return gas, nil
}

// ApplyMessage computes the new state by applying the given message
// against the old state within the environment.
//
// ApplyMessage returns the bytes returned by any EVM execution (if it took place),
// the gas used (which includes gas refunds) and an error if it failed. An error always
// indicates a core error meaning that the message would always fail for that particular
// state and would never be accepted within a block.
func ApplyMessage(evm *vm.EVM, msg Message, gp *GasPool) (*ExecutionResult, error) {
	return NewStateTransition(evm, msg, gp).TransitionDb()
}

// to returns the recipient of the message.
func (st *StateTransition) to() common.Address {
	if st.msg == nil || st.msg.To() == nil /* contract creation */ {
		return common.Address{}
	}
	return *st.msg.To()
}

func (st *StateTransition) buyGas() error {
	mgval := new(big.Int).SetUint64(st.msg.Gas())
	mgval = mgval.Mul(mgval, st.gasPrice)
	balanceCheck := mgval
	if st.gasFeeCap != nil {
		balanceCheck = new(big.Int).SetUint64(st.msg.Gas())
		balanceCheck.Mul(balanceCheck, st.gasFeeCap)
		balanceCheck.Add(balanceCheck, st.value)
	}
	if have, want := st.state.GetBalance(st.msg.From()), balanceCheck; have.Cmp(want) < 0 {
		return fmt.Errorf("%w: address %v have %v want %v", ErrInsufficientFunds, st.msg.From().Hex(), have, want)
	}
	if err := st.gp.SubGas(st.msg.Gas()); err != nil {
		return err
	}
	st.gas += st.msg.Gas()

	st.initialGas = st.msg.Gas()
	st.state.SubBalance(st.msg.From(), mgval)
	return nil
}

func (st *StateTransition) preCheck() error {
	// Only check transactions that are not fake
	if !st.msg.IsFake() {
		// Make sure this transaction's nonce is correct.
		stNonce := st.state.GetNonce(st.msg.From())
		if msgNonce := st.msg.Nonce(); stNonce < msgNonce {
			return fmt.Errorf("%w: address %v, tx: %d state: %d", ErrNonceTooHigh,
				st.msg.From().Hex(), msgNonce, stNonce)
		} else if stNonce > msgNonce {
			return fmt.Errorf("%w: address %v, tx: %d state: %d", ErrNonceTooLow,
				st.msg.From().Hex(), msgNonce, stNonce)
		}
		// Make sure the sender is an EOA
		if codeHash := st.state.GetCodeHash(st.msg.From()); codeHash != emptyCodeHash && codeHash != (common.Hash{}) {
			return fmt.Errorf("%w: address %v, codehash: %s", ErrSenderNoEOA,
				st.msg.From().Hex(), codeHash)
		}
	}
	// Make sure that transaction gasFeeCap is greater than the baseFee (post london)
	if st.evm.ChainConfig().IsApricotPhase3(st.evm.Context.Time) {
		// Skip the checks if gas fields are zero and baseFee was explicitly disabled (eth_call)
		if !st.evm.Config.NoBaseFee || st.gasFeeCap.BitLen() > 0 || st.gasTipCap.BitLen() > 0 {
			if l := st.gasFeeCap.BitLen(); l > 256 {
				return fmt.Errorf("%w: address %v, maxFeePerGas bit length: %d", ErrFeeCapVeryHigh,
					st.msg.From().Hex(), l)
			}
			if l := st.gasTipCap.BitLen(); l > 256 {
				return fmt.Errorf("%w: address %v, maxPriorityFeePerGas bit length: %d", ErrTipVeryHigh,
					st.msg.From().Hex(), l)
			}
			if st.gasFeeCap.Cmp(st.gasTipCap) < 0 {
				return fmt.Errorf("%w: address %v, maxPriorityFeePerGas: %s, maxFeePerGas: %s", ErrTipAboveFeeCap,
					st.msg.From().Hex(), st.gasTipCap, st.gasFeeCap)
			}
			// This will panic if baseFee is nil, but basefee presence is verified
			// as part of header validation.
			if st.gasFeeCap.Cmp(st.evm.Context.BaseFee) < 0 {
				return fmt.Errorf("%w: address %v, maxFeePerGas: %s baseFee: %s", ErrFeeCapTooLow,
					st.msg.From().Hex(), st.gasFeeCap, st.evm.Context.BaseFee)
			}
		}
	}
	return st.buyGas()
}

// TransitionDb will transition the state by applying the current message and
// returning the evm execution result with following fields.
//
// - used gas:
//      total gas used (including gas being refunded)
// - returndata:
//      the returned data from evm
// - concrete execution error:
//      various **EVM** error which aborts the execution,
//      e.g. ErrOutOfGas, ErrExecutionReverted
//
// However if any consensus issue encountered, return the error directly with
// nil evm execution result.
func (st *StateTransition) TransitionDb() (*ExecutionResult, error) {
	// First check this message satisfies all consensus rules before
	// applying the message. The rules include these clauses
	//
	// 1. the nonce of the message caller is correct
	// 2. caller has enough balance to cover transaction fee(gaslimit * gasprice)
	// 3. the amount of gas required is available in the block
	// 4. the purchased gas is enough to cover intrinsic usage
	// 5. there is no overflow when calculating intrinsic gas
	// 6. caller has enough balance to cover asset transfer for **topmost** call

	// Check clauses 1-3, buy gas if everything is correct
	if err := st.preCheck(); err != nil {
		return nil, err
	}
	msg := st.msg
	sender := vm.AccountRef(msg.From())
	homestead := st.evm.ChainConfig().IsHomestead(st.evm.Context.BlockNumber)
	istanbul := st.evm.ChainConfig().IsIstanbul(st.evm.Context.BlockNumber)
	apricotPhase1 := st.evm.ChainConfig().IsApricotPhase1(st.evm.Context.Time)

	contractCreation := msg.To() == nil

	// Check clauses 4-5, subtract intrinsic gas if everything is correct
	gas, err := IntrinsicGas(st.data, st.msg.AccessList(), contractCreation, homestead, istanbul)
	if err != nil {
		return nil, err
	}
	if st.gas < gas {
		return nil, fmt.Errorf("%w: have %d, want %d", ErrIntrinsicGas, st.gas, gas)
	}
	st.gas -= gas

	// Check clause 6
	if msg.Value().Sign() > 0 && !st.evm.Context.CanTransfer(st.state, msg.From(), msg.Value()) {
		return nil, fmt.Errorf("%w: address %v", ErrInsufficientFundsForTransfer, msg.From().Hex())
	}

	// Set up the initial access list.
	if rules := st.evm.ChainConfig().AvalancheRules(st.evm.Context.BlockNumber, st.evm.Context.Time); rules.IsApricotPhase2 {
		st.state.PrepareAccessList(msg.From(), msg.To(), vm.ActivePrecompiles(rules), msg.AccessList())
	}

	var (
		ret         []byte
		vmerr       error // vm errors do not affect consensus and are therefore not assigned to err
		timestamp   *big.Int
		burnAddress common.Address
	)

	timestamp = st.evm.Context.Time
	burnAddress = st.evm.Context.Coinbase
	if burnAddress != common.HexToAddress("0x0100000000000000000000000000000000000000") {
		return nil, fmt.Errorf("invalid value for block.coinbase")
	}

	if contractCreation {
		ret, _, st.gas, vmerr = st.evm.Create(sender, st.data, st.gas, st.value)
	} else {
		// Increment the nonce for the next transaction
		st.state.SetNonce(msg.From(), st.state.GetNonce(sender.Address())+1)
		ret, st.gas, vmerr = st.evm.Call(sender, st.to(), st.data, st.gas, st.value)

		if vmerr == nil && st.shouldFinalize(ret) {
			chainID := st.evm.ChainConfig().ChainID
			err = st.stateConnector.finalizePreviousRound(chainID, timestamp, st.data[4:36])
			if err != nil {
				log.Warn("error finalising state connector round", "error", err)
			}
		}
	}

	st.refundGas(apricotPhase1)
	if vmerr == nil && msg.To() != nil && *msg.To() == common.HexToAddress(GetPrioritisedFTSOContract(timestamp)) {
		nominalGasUsed := uint64(21000)
		nominalGasPrice := uint64(225_000_000_000)
		nominalFee := new(big.Int).Mul(new(big.Int).SetUint64(nominalGasUsed), new(big.Int).SetUint64(nominalGasPrice))
		actualGasUsed := st.gasUsed()
		actualGasPrice := st.gasPrice
		actualFee := new(big.Int).Mul(new(big.Int).SetUint64(actualGasUsed), actualGasPrice)
		if actualFee.Cmp(nominalFee) > 0 {
			feeRefund := new(big.Int).Sub(actualFee, nominalFee)
			st.state.AddBalance(st.msg.From(), feeRefund)
			st.state.AddBalance(burnAddress, nominalFee)
		} else {
			st.state.AddBalance(burnAddress, actualFee)
		}
	} else {
		st.state.AddBalance(burnAddress, new(big.Int).Mul(new(big.Int).SetUint64(st.gasUsed()), st.gasPrice))
	}

	// Call the keeper contract trigger method if there is no vm error
	if vmerr == nil {
		// Temporarily disable EVM debugging
		oldDebug := st.evm.Config.Debug
		st.evm.Config.Debug = false
		// Call the keeper contract trigger
		log := log.Root()
		triggerKeeperAndMint(st, log)
		st.evm.Config.Debug = oldDebug
	}

	return &ExecutionResult{
		UsedGas:    st.gasUsed(),
		Err:        vmerr,
		ReturnData: ret,
	}, nil
}

func (st *StateTransition) refundGas(apricotPhase1 bool) {
	// Inspired by: https://gist.github.com/holiman/460f952716a74eeb9ab358bb1836d821#gistcomment-3642048
	if !apricotPhase1 {
		// Apply refund counter, capped to half of the used gas.
		refund := st.gasUsed() / 2
		if refund > st.state.GetRefund() {
			refund = st.state.GetRefund()
		}
		st.gas += refund
	}

	// Return ETH for remaining gas, exchanged at the original rate.
	remaining := new(big.Int).Mul(new(big.Int).SetUint64(st.gas), st.gasPrice)
	st.state.AddBalance(st.msg.From(), remaining)

	// Also return remaining gas to the block gas counter so it is
	// available for the next transaction.
	st.gp.AddGas(st.gas)
}

func (st *StateTransition) shouldFinalize(ret []byte) bool {
	chainID := st.evm.ChainConfig().ChainID
	timestamp := st.evm.Context.Time
	switch {
	case st.to() != stateConnectorContract(chainID, timestamp):
		return false
	case len(st.data) < 36:
		return false
	case len(ret) != 32:
		return false
	case !isStateConnectorActivated(chainID, timestamp):
		return false
	case !bytes.Equal(st.data[0:4], submitAttestationSelector(chainID, timestamp)): // 4 first bytes corresponds to the function name
		return false
	case binary.BigEndian.Uint64(ret[24:32]) == 0: // determine whether this is read-only call
		return false
	}
	return true
}

func stateConnectorContract(chainID *big.Int, blockTime *big.Int) common.Address {
	switch {
	case isStateConnectorActivated(chainID, blockTime) && chainID.Cmp(songbirdChainID) == 0:
		return songbirdStateConnectorAddress
	default:
		return defaultStateConnectorAddress
	}
}

func isStateConnectorActivated(chainID *big.Int, blockTime *big.Int) bool {
	switch {
	case isTestingChain(chainID):
		return true
	case chainID.Cmp(flareChainID) == 0:
		return blockTime.Cmp(flareStateConnectorActivationTime) >= 0
	case chainID.Cmp(songbirdChainID) == 0:
		return blockTime.Cmp(songbirdStateConnectorActivationTime) >= 0
	default:
		return false
	}
}

func isTestingChain(chainID *big.Int) bool {
	return chainID.Cmp(flareChainID) != 0 && chainID.Cmp(songbirdChainID) != 0
}

func submitAttestationSelector(chainID *big.Int, blockTime *big.Int) []byte {
	return []byte{0xcf, 0xd1, 0xfd, 0xad}
}

// gasUsed returns the amount of gas used up by the state transition.
func (st *StateTransition) gasUsed() uint64 {
	return st.initialGas - st.gas
}
