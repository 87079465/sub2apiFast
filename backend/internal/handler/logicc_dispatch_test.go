package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestLogiccMessagesDispatchAndModelCatalog(t *testing.T) {
	key := &service.APIKey{Group: &service.Group{Platform: service.PlatformLogicc}}
	require.True(t, allowOpenAICompatibleMessagesDispatch(nil, key))
	require.Equal(t, []string{"default"}, defaultModelIDsForPlatform(service.PlatformLogicc))
	require.Equal(t, service.OpenAIEndpointCapabilityChatCompletions, openAIResponsesRequiredCapability(false, service.PlatformLogicc))
}
