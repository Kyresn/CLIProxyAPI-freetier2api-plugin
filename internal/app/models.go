package app

// 本文件是 model provider 能力的 ABI 适配层。

import (
	"context"
	"net/http"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

type staticModelRPCRequest struct {
	pluginapi.StaticModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type authModelRPCRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (a *App) handleModelStatic(request []byte) ([]byte, error) {
	var rpc staticModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostbridge.WithCallbackID(context.Background(), rpc.HostCallbackID)
	models, errModels := a.models.StaticModels(ctx)
	if errModels != nil {
		return nil, core.ToPluginError(errModels)
	}
	return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey, Models: models})
}

func (a *App) handleModelRegister(request []byte) ([]byte, error) {
	var rpc staticModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostbridge.WithCallbackID(context.Background(), rpc.HostCallbackID)
	models, errModels := a.models.StaticModels(ctx)
	if errModels != nil {
		return nil, core.ToPluginError(errModels)
	}
	return core.OKEnvelope(pluginapi.ModelRegistrationResponse{Provider: providerKey, Models: models})
}

func (a *App) handleModelForAuth(request []byte) ([]byte, error) {
	var rpc authModelRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostbridge.WithCallbackID(context.Background(), rpc.HostCallbackID)
	credential, vendor, errResolve := a.resolveVendorCredential(ctx, rpc.HostCallbackID, rpc.StorageJSON, rpc.AuthID, rpc.Attributes)
	if errResolve != nil {
		logger.Debug("model.for_auth: no credential for %s: %v", rpc.AuthID, errResolve)
		return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	if !a.models.RealmEnabled(vendor.Region()) {
		return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	models, errModels := core.ModelsFor(vendor, ctx, credential)
	if errModels != nil {
		logger.Debug("model.for_auth: probe models for %s failed: %v", vendor.ID(), errModels)
		return core.OKEnvelope(pluginapi.ModelResponse{Provider: providerKey})
	}
	// 顺手写入控制台回读缓存（官方 ID，未过滤/未换别名）。
	//
	// 宿主加载每个凭证文件都会走这条 RPC，因此插件启动后不再依赖 static 探测
	// 的时序：控制台列表不会因为探测还没跑完而缺整家供应商。
	// 空清单不写缓存：供应商在探测失败时按契约返回空结果（让宿主淘汰坏凭证），
	// 若照样覆盖，一次瞬时失败就会把整家供应商从控制台抹掉。
	if !a.models.CacheProbedModels(vendor.ID(), models) {
		logger.Info("model.for_auth: %s returned no models for %s (host will drop this credential)",
			vendor.ID(), rpc.AuthID)
	}
	return core.OKEnvelope(pluginapi.ModelResponse{
		Provider: providerKey,
		Models:   a.models.ApplyAliases(vendor.ID(), a.models.FilterForVendor(vendor.ID(), models)),
	})
}

func (a *App) applyModelCatalog(_ pluginConfig) {
	a.models.ClearRestartPending()
	snapshot := a.SnapshotState()
	a.models.ReloadFromState(snapshot)
	a.models.LoadCachedState(snapshot.Models)
}

func (a *App) refreshModelsFromUpstream(ctx context.Context) (int, error) {
	total, errRefresh := a.models.RefreshFromUpstream(ctx)
	if errRefresh != nil {
		return 0, core.ToPluginError(errRefresh)
	}
	return total, nil
}

func (a *App) resolveOutboundModel(vendorID, model string) string {
	return a.models.ResolveOutboundModel(vendorID, model)
}

func (a *App) applyModelAliases(vendorID string, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	return a.models.ApplyAliases(vendorID, models)
}

func (a *App) isModelDisabled(vendorID, modelID string) bool {
	return a.models.IsDisabled(vendorID, modelID)
}
