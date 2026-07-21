package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestHoneyAttestationNeverUsesResponsesGlobalConversion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request.Header.Set("X-NewAPI-Attestation-Version", "2")

	info := &relaycommon.RelayInfo{
		OriginModelName: "honey-claude-sonnet-4-6",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:   15,
			ChannelType: 14,
		},
	}
	require.False(t, shouldUseResponsesGlobal(c, info))
}

func TestHoneyAttestedTerminalIsOneTypedEnvelopeFollowedByDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	require.NoError(t, finishHoneyAttestedStream(
		c,
		relaycommon.NewHoneyAttestedErrorEnvelope(
			&relaycommon.RelayInfo{HoneyAttestedRelay: &relaycommon.HoneyAttestedRelay{AttemptID: "attempt-1"}},
			"settlement_failed",
		),
	))

	body := recorder.Body.String()
	require.Equal(t, 2, strings.Count(body, "data: "))
	require.Contains(t, body, `"object":"newapi.attested_relay.error"`)
	require.Contains(t, body, `"code":"settlement_failed"`)
	require.True(t, strings.HasSuffix(body, "data: [DONE]\n\n"))
	require.True(t, relaycommon.HoneyAttestedTerminalWritten(c))
}
