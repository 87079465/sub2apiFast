package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestOfficeCopilotMessagesDispatch(t *testing.T) {
	key := &service.APIKey{Group: &service.Group{Platform: service.PlatformOfficeCopilot}}
	require.True(t, allowOpenAICompatibleMessagesDispatch(nil, key))
	key.Group.Platform = service.PlatformOpenAI
	require.False(t, allowOpenAICompatibleMessagesDispatch(nil, key))
}
