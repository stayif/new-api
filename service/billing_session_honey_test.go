package service

import (
	"errors"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/require"
)

type honeySettlementFunding struct {
	settledDeltas []int
	settleErr     error
}

func (f *honeySettlementFunding) Source() string       { return BillingSourceWallet }
func (f *honeySettlementFunding) PreConsume(int) error { return nil }
func (f *honeySettlementFunding) Refund() error        { return nil }
func (f *honeySettlementFunding) Settle(delta int) error {
	f.settledDeltas = append(f.settledDeltas, delta)
	return f.settleErr
}

func TestHoneyAttestedBillingSessionCommitsFundingAsSettlement(t *testing.T) {
	funding := &honeySettlementFunding{}
	session := &BillingSession{
		relayInfo: &relaycommon.RelayInfo{
			HoneyAttestedRelay: &relaycommon.HoneyAttestedRelay{},
			IsPlayground:       true,
		},
		funding:          funding,
		preConsumedQuota: 4,
	}

	require.NoError(t, session.Settle(9))
	require.Equal(t, []int{5}, funding.settledDeltas)
	require.True(t, session.fundingSettled)
	require.True(t, session.settled)
}

func TestHoneyAttestedBillingSessionFailsClosedWhenFundingDoesNotSettle(t *testing.T) {
	settlementErr := errors.New("settlement unavailable")
	funding := &honeySettlementFunding{settleErr: settlementErr}
	session := &BillingSession{
		relayInfo: &relaycommon.RelayInfo{
			HoneyAttestedRelay: &relaycommon.HoneyAttestedRelay{},
			IsPlayground:       true,
		},
		funding:          funding,
		preConsumedQuota: 4,
	}

	require.ErrorIs(t, session.Settle(9), settlementErr)
	require.Equal(t, []int{5}, funding.settledDeltas)
	require.False(t, session.fundingSettled)
	require.False(t, session.settled)
}
