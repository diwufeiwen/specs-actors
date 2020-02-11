package paych

import (
	"bytes"
	"fmt"
	"sort"

	addr "github.com/filecoin-project/go-address"

	abi "github.com/filecoin-project/specs-actors/actors/abi"
	big "github.com/filecoin-project/specs-actors/actors/abi/big"
	"github.com/filecoin-project/specs-actors/actors/builtin"
	acrypto "github.com/filecoin-project/specs-actors/actors/crypto"
	vmr "github.com/filecoin-project/specs-actors/actors/runtime"
	"github.com/filecoin-project/specs-actors/actors/runtime/exitcode"
	adt "github.com/filecoin-project/specs-actors/actors/util/adt"
)

// Maximum number of lanes in a channel.
const LaneLimit = 256

const SettleDelay = abi.ChainEpoch(1) // placeholder PARAM_FINISH

type Actor struct{}

func (a Actor) Exports() []interface{} {
	return []interface{}{
		builtin.MethodConstructor: a.Constructor,
		2:                         a.UpdateChannelState,
		3:                         a.Settle,
		4:                         a.Collect,
	}
}

var _ abi.Invokee = Actor{}

type ConstructorParams struct {
	From addr.Address // Payer
	To addr.Address   // Payee
}

// Constructor creates a payment channel actor. See State for meaning of params.
func (pca *Actor) Constructor(rt vmr.Runtime, params *ConstructorParams) *adt.EmptyValue {
	// Only InitActor can create a payment channel actor. It creates the actor on
	// behalf of the payer/payee.
	rt.ValidateImmediateCallerType(builtin.InitActorCodeID)

	// check that both parties are capable of signing vouchers
	err := pca.validateActor(rt, params.To)
	if err != nil {
		rt.Abortf(exitcode.ErrIllegalArgument, err.Error())
	}
	err = pca.validateActor(rt, params.From)
	if err != nil {
		rt.Abortf(exitcode.ErrIllegalArgument, err.Error())
	}

	st := ConstructState(params.From, params.To)
	rt.State().Create(st)

	return &adt.EmptyValue{}
}

// validateActor requires an actor to be an account actor and to have a canonical ID address.
// The account actor constructor checks that the embedded address is associated
// with an appropriate key.
// An alternative (more expensive) would be to send a message to the actor to fetch its key.
func (pca *Actor) validateActor(rt vmr.Runtime, actorAddr addr.Address) error {
	codeCID, ok := rt.GetActorCodeCID(actorAddr)
	if !ok {
		return fmt.Errorf("no code for address %v", actorAddr)
	}
	if codeCID != builtin.AccountActorCodeID {
		return fmt.Errorf("actor %v must be an account (%v), was %v",
			actorAddr, builtin.AccountActorCodeID, codeCID)
	}
	// Check that target is a canonical ID address.
	// This is required for consistent caller validation.
	if actorAddr.Protocol() != addr.ID {
		return fmt.Errorf("address must be an ID-address, %v is %v", actorAddr, actorAddr.Protocol())
	}
	return nil
}

////////////////////////////////////////////////////////////////////////////////
// Payment Channel state operations
////////////////////////////////////////////////////////////////////////////////

type UpdateChannelStateParams struct {
	Sv     SignedVoucher
	Secret []byte
	Proof  []byte
}

// A voucher is sent by `From` to `To` off-chain in order to enable
// `To` to redeem payments on-chain in the future
type SignedVoucher struct {
	// TimeLock sets a min epoch before which the voucher cannot be redeemed
	TimeLock abi.ChainEpoch
	// (optional) The SecretPreImage is used by `To` to validate
	SecretPreimage []byte
	// (optional) Extra can be specified by `From` to add a verification method to the voucher
	Extra *ModVerifyParams
	// Specifies which lane the Voucher merges into (will be created if does not exist)
	Lane int64
	// Nonce is set by `From` to prevent redemption of stale vouchers on a lane
	Nonce int64
	// Amount voucher can be redeemed for
	Amount big.Int
	// (optional) MinSettleHeight can extend channel MinSettleHeight if needed
	MinSettleHeight abi.ChainEpoch

	// (optional) Set of lanes to be merged into `Lane`
	Merges []Merge

	// Sender's signature over the voucher
	Signature *acrypto.Signature
}

// Modular Verification method
type ModVerifyParams struct {
	Actor  addr.Address
	Method abi.MethodNum
	Data   []byte
}

type PaymentVerifyParams struct {
	Extra []byte
	Proof []byte
}

func (pca Actor) UpdateChannelState(rt vmr.Runtime, params *UpdateChannelStateParams) *adt.EmptyValue {
	var st State
	rt.State().Readonly(&st)

	// both parties must sign voucher: one who submits it, the other explicitly signs it
	rt.ValidateImmediateCallerIs(st.From, st.To)
	var signer addr.Address
	if rt.Message().Caller() == st.From {
		signer = st.To
	} else {
		signer = st.From
	}
	sv := params.Sv

	if sv.Signature == nil {
		rt.Abortf(exitcode.ErrIllegalArgument, "voucher has no signature")
	}

	vb, nerr := sv.SigningBytes()
	if nerr != nil {
		rt.Abortf(exitcode.ErrIllegalArgument, "failed to serialize signedvoucher")
	}

	if !rt.Syscalls().VerifySignature(*sv.Signature, signer, vb) {
		rt.Abortf(exitcode.ErrIllegalArgument, "voucher signature invalid")
	}

	if rt.CurrEpoch() < sv.TimeLock {
		rt.Abortf(exitcode.ErrIllegalArgument, "cannot use this voucher yet!")
	}

	if len(sv.SecretPreimage) > 0 {
		if !bytes.Equal(rt.Syscalls().Hash_SHA256(params.Secret), sv.SecretPreimage) {
			rt.Abortf(exitcode.ErrIllegalArgument, "incorrect secret!")
		}
	}

	if sv.Extra != nil {

		_, code := rt.Send(
			sv.Extra.Actor,
			sv.Extra.Method,
			&PaymentVerifyParams{
				sv.Extra.Data,
				params.Proof,
			},
			abi.NewTokenAmount(0),
		)
		builtin.RequireSuccess(rt, code, "spend voucher verification failed")
	}

	rt.State().Transaction(&st, func() interface{} {
		// Find the voucher lane, create and insert it in sorted order if necessary.
		laneIdx, ls := findLane(st.LaneStates, sv.Lane)
		if ls == nil {
			if len(st.LaneStates) >= LaneLimit {
				rt.Abortf(exitcode.ErrIllegalArgument, "lane limit exceeded")
			}
			ls = &LaneState{
				ID:       sv.Lane,
				Redeemed: big.NewInt(0),
				Nonce:    0,
			}
			st.LaneStates = append(st.LaneStates[:laneIdx], append([]*LaneState{ls}, st.LaneStates[laneIdx:]...)...)

		}

		if ls.Nonce > sv.Nonce {
			rt.Abortf(exitcode.ErrIllegalArgument, "voucher has an outdated nonce, cannot redeem")
		}

		// The next section actually calculates the payment amounts to update the payment channel state
		// 1. (optional) sum already redeemed value of all merging lanes
		redeemedFromOthers := big.Zero()
		for _, merge := range sv.Merges {
			if merge.Lane == sv.Lane {
				rt.Abortf(exitcode.ErrIllegalArgument, "voucher cannot merge lanes into its own lane")
			}

			_, otherls := findLane(st.LaneStates, merge.Lane)
			if otherls != nil {
				if otherls.Nonce >= merge.Nonce {
					rt.Abortf(exitcode.ErrIllegalArgument, "merged lane in voucher has outdated nonce, cannot redeem")
				}

				redeemedFromOthers = big.Add(redeemedFromOthers, otherls.Redeemed)
				otherls.Nonce = merge.Nonce
			} else {
				rt.Abortf(exitcode.ErrIllegalArgument, "voucher specifies invalid merge lane %v", merge.Lane)
			}
		}

		// 2. To prevent double counting, remove already redeemed amounts (from
		// voucher or other lanes) from the voucher amount
		ls.Nonce = sv.Nonce
		balanceDelta := big.Sub(sv.Amount, big.Add(redeemedFromOthers, ls.Redeemed))
		// 3. set new redeemed value for merged-into lane
		ls.Redeemed = sv.Amount

		newSendBalance := big.Add(st.ToSend, balanceDelta)

		// 4. check operation validity
		if newSendBalance.LessThan(big.Zero()) {
			rt.Abortf(exitcode.ErrIllegalState, "voucher would leave channel balance negative")
		}
		if newSendBalance.GreaterThan(rt.CurrentBalance()) {
			rt.Abortf(exitcode.ErrIllegalState, "not enough funds in channel to cover voucher")
		}

		// 5. add new redemption ToSend
		st.ToSend = newSendBalance

		// update channel settlingAt and MinSettleHeight if delayed by voucher
		if sv.MinSettleHeight != 0 {
			if st.SettlingAt != 0 && st.SettlingAt < sv.MinSettleHeight {
				st.SettlingAt = sv.MinSettleHeight
			}
			if st.MinSettleHeight < sv.MinSettleHeight {
				st.MinSettleHeight = sv.MinSettleHeight
			}
		}
		return nil
	})
	return &adt.EmptyValue{}
}

func (pca Actor) Settle(rt vmr.Runtime, _ *adt.EmptyValue) *adt.EmptyValue {
	var st State
	rt.State().Transaction(&st, func() interface{} {

		rt.ValidateImmediateCallerIs(st.From, st.To)

		if st.SettlingAt != 0 {
			rt.Abortf(exitcode.ErrIllegalState, "channel already settling")
		}

		st.SettlingAt = rt.CurrEpoch() + SettleDelay
		if st.SettlingAt < st.MinSettleHeight {
			st.SettlingAt = st.MinSettleHeight
		}

		return nil
	})
	return &adt.EmptyValue{}
}

func (pca Actor) Collect(rt vmr.Runtime, _ *adt.EmptyValue) *adt.EmptyValue {
	var st State
	rt.State().Readonly(&st)
	rt.ValidateImmediateCallerIs(st.From, st.To)

	if st.SettlingAt == 0 || rt.CurrEpoch() < st.SettlingAt {
		rt.Abortf(exitcode.ErrForbidden, "payment channel not settling or settled")
	}

	// send remaining balance to "From"

	_, codeFrom := rt.Send(
		st.From,
		builtin.MethodSend,
		nil,
		abi.NewTokenAmount(big.Sub(rt.CurrentBalance(), st.ToSend).Int64()),
	)
	builtin.RequireSuccess(rt, codeFrom, "Failed to send balance to `From`")

	// send ToSend to "To"

	_, codeTo := rt.Send(
		st.From,
		builtin.MethodSend,
		nil,
		abi.NewTokenAmount(st.ToSend.Int64()),
	)
	builtin.RequireSuccess(rt, codeTo, "Failed to send funds to `To`")

	rt.State().Transaction(&st, func() interface{} {
		st.ToSend = big.Zero()
		return nil
	})
	return &adt.EmptyValue{}
}

func (t *SignedVoucher) SigningBytes() ([]byte, error) {
	osv := *t
	osv.Signature = nil

	buf := new(bytes.Buffer)
	if err := osv.MarshalCBOR(buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// Returns the insertion index for a lane ID, with the matching lane state if found, or nil.
func findLane(lanes []*LaneState, ID int64) (int, *LaneState) {
	insertionIdx := sort.Search(len(lanes), func(i int) bool {
		return lanes[i].ID >= ID
	})
	if insertionIdx == len(lanes) || lanes[insertionIdx].ID != int64(insertionIdx) {
		// Not found
		return insertionIdx, nil
	}
	return insertionIdx, lanes[insertionIdx]
}