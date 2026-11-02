package jsplugin

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/model"
)

// DeleteTask is an optional host capability. The plugin supplies only the
// request shape; credentials are removed from its context and reapplied by the
// host from the executing channel before the request is sent.
func (a *TaskAdaptor) DeleteTask(baseURL, key string, task *model.Task, proxy string) (*http.Response, error) {
	officialArkDelete := a.plugin != nil && a.plugin.Meta.Key == "doubao_ark_official"
	if officialArkDelete && (task == nil || task.ProviderTaskID == "") {
		return nil, fmt.Errorf("provider task ID is required for official Ark deletion")
	}
	if task != nil && task.PrivateData.Key != "" {
		key = task.PrivateData.Key
	}
	ctx, err := a.queryContext(task, key, baseURL, proxy)
	if err != nil {
		return nil, err
	}
	if officialArkDelete {
		ctx["taskId"] = task.ProviderTaskID
	}
	authHeader, _ := ctx["authHeader"].(string)
	delete(ctx, "apiKey")
	delete(ctx, "authHeader")
	delete(ctx, "auth")
	descriptor, err := a.requestDescriptor(context.Background(), "buildDeleteRequest", ctx)
	if err != nil {
		return nil, err
	}
	if authHeader != "" {
		if !strings.Contains(authHeader, " ") {
			authHeader = "Bearer " + authHeader
		}
		if descriptor.Headers == nil {
			descriptor.Headers = make(map[string]string)
		}
		descriptor.Headers["Authorization"] = authHeader
	}
	return a.doFetchDescriptor(baseURL, key, proxy, descriptor)
}
