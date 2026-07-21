package relay

import (
	"net/http"
	"net/http/httptest"
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
