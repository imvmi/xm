package web

import "slices"

type apiScopeDefinition struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Group       string `json:"group"`
	Description string `json:"description"`
}

// Shared by credential editing and the public API documentation.
var apiScopeDefinitions = []apiScopeDefinition{
	{"accounts:read", "读取账号", "账号", "查询授权账号列表和详情"},
	{"accounts:create", "添加账号", "账号", "添加监控账号，并指定初始采集设置"},
	{"accounts:update", "修改账号", "账号", "修改已有账号的自动采集和包含回复设置"},
	{"accounts:delete", "取消监控", "账号", "取消监控并保留归档"},
	{"tasks:read", "读取任务", "采集任务", "查询本凭据提交的任务列表和进度"},
	{"tasks:submit:incremental", "增量采集", "采集任务", "提交增量任务；首次采集仍会建立历史基线"},
	{"tasks:submit:backfill", "历史补采", "采集任务", "提交历史补采任务，继续已有进度"},
	{"tasks:submit:rescan", "重新扫描", "采集任务", "重置采集进度并重新扫描历史"},
	{"tasks:retry", "重试任务", "采集任务", "重试本凭据的任务；还需对应采集模式权限，重新扫描的重试需历史补采权限"},
	{"tasks:cancel", "取消任务", "采集任务", "取消本凭据提交的任务"},
	{"timeline:list", "读取时间线", "归档内容", "按账号查询归档时间线及引用"},
	{"posts:read", "读取单条推文", "归档内容", "按推文 ID 查询归档正文及引用"},
	{"media:download", "读取媒体文件", "归档内容", "下载归档图片、视频和动图，支持 Range"},
	{"avatars:read", "读取账号头像", "归档内容", "读取授权账号的头像文件"},
}

var apiScopes = func() []string {
	scopes := make([]string, len(apiScopeDefinitions))
	for i, scope := range apiScopeDefinitions {
		scopes[i] = scope.ID
	}
	return scopes
}()

// Expand old bundled scopes at the storage boundary. Saving an edited
// credential persists only the individual scopes, so unchecked grants stay off.
var legacyAPIScopes = map[string][]string{
	"accounts:write": {"accounts:create", "accounts:update"},
	"tasks:create":   {"tasks:submit:incremental", "tasks:submit:backfill", "tasks:retry"},
	"tasks:rescan":   {}, // Previously effective only together with tasks:create.
	"timeline:read":  {"timeline:list", "posts:read"},
	"media:read":     {"media:download", "avatars:read"},
}

func expandAPIScopes(scopes []string) []string {
	result := []string{}
	for _, scope := range scopes {
		if expanded, legacy := legacyAPIScopes[scope]; legacy {
			result = append(result, expanded...)
		} else {
			result = append(result, scope)
		}
	}
	if slices.Contains(scopes, "tasks:create") && slices.Contains(scopes, "tasks:rescan") {
		result = append(result, "tasks:submit:rescan")
	}
	slices.Sort(result)
	return slices.Compact(result)
}

func apiTaskScope(mode string) string { return "tasks:submit:" + mode }
