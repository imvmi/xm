package web

import (
	"net/http"
	"reflect"
	"strings"
)

func (a *App) apiDocs(w http.ResponseWriter, r *http.Request) {
	b, err := static.ReadFile("static/api-docs.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}
func apiSchema(value any) map[string]any {
	t := reflect.TypeOf(value)
	props := map[string]any{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		typ := "string"
		switch f.Type.Kind() {
		case reflect.Bool:
			typ = "boolean"
		case reflect.Int, reflect.Int64:
			typ = "integer"
		case reflect.Slice:
			typ = "array"
		}
		p := map[string]any{"type": typ}
		if typ == "array" {
			p["items"] = map[string]string{"type": "string"}
		}
		props[tag] = p
	}
	return map[string]any{"type": "object", "properties": props}
}
func apiRef(name string) map[string]string {
	return map[string]string{"$ref": "#/components/schemas/" + name}
}
func apiJSONResponse(description string, schema any) map[string]any {
	return map[string]any{"description": description, "content": map[string]any{"application/json": map[string]any{"schema": schema}}}
}
func apiPageSchema(item string) map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"items": map[string]any{"type": "array", "items": apiRef(item)}, "has_more": map[string]string{"type": "boolean"}, "next_cursor": map[string]string{"type": "string"}}}
}
func (a *App) apiOpenAPI(w http.ResponseWriter, r *http.Request) { respond(w, 200, externalOpenAPI()) }
func externalOpenAPI() map[string]any {
	paths := map[string]any{}
	for _, route := range externalRoutes {
		method, path, _ := strings.Cut(route.Pattern, " ")
		parameters := []any{}
		for _, segment := range strings.Split(path, "/") {
			if strings.HasPrefix(segment, "{") {
				parameters = append(parameters, map[string]any{"name": strings.Trim(segment, "{}"), "in": "path", "required": true, "schema": map[string]string{"type": "string"}})
			}
		}
		if route.Idempotent {
			parameters = append(parameters, map[string]any{"name": "Idempotency-Key", "in": "header", "required": true, "description": "每个凭据独立；相同请求使用同一键，保留 24 小时；不同请求不得复用。", "schema": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}})
		}
		addQuery := func(name string, schema any) {
			parameters = append(parameters, map[string]any{"name": name, "in": "query", "schema": schema})
		}
		isList := route.Pattern == "GET /api/v1/accounts" || route.Pattern == "GET /api/v1/tasks" || strings.HasSuffix(path, "/timeline")
		if isList {
			addQuery("limit", map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 50})
			addQuery("cursor", map[string]string{"type": "string"})
		}
		if route.Pattern == "GET /api/v1/tasks" {
			addQuery("account_id", map[string]string{"type": "string"})
			addQuery("status", map[string]string{"type": "string"})
			addQuery("created_after", map[string]any{"type": "integer", "minimum": 0})
			addQuery("created_before", map[string]any{"type": "integer", "minimum": 0})
		}
		if strings.HasSuffix(path, "/timeline") {
			addQuery("section", map[string]any{"type": "string", "enum": []string{"posts", "replies", "media"}})
			addQuery("kind", map[string]any{"type": "string", "enum": []string{"photo", "video", "animated_gif"}})
			addQuery("q", map[string]any{"type": "string", "maxLength": 500})
			addQuery("count", map[string]any{"type": "string", "enum": []string{"0", "1"}, "default": "0"})
		}
		responses := map[string]any{}
		for _, code := range []string{"400", "401", "403", "404", "409", "429", "500", "503", "504"} {
			responses[code] = apiJSONResponse("请求失败；error.code 为机器可读错误码，error.request_id 用于排查。", apiRef("Error"))
		}
		responseSchema := any(map[string]string{"type": "object"})
		success := "200"
		switch route.Pattern {
		case "GET /api/v1/me":
			responseSchema = apiRef("Credential")
		case "GET /api/v1/accounts":
			responseSchema = apiPageSchema("Account")
		case "GET /api/v1/accounts/{account}":
			responseSchema = apiRef("Account")
		case "POST /api/v1/accounts":
			success = "201"
			responseSchema = map[string]any{"type": "object", "properties": map[string]any{"id": map[string]string{"type": "string"}, "username": map[string]string{"type": "string"}, "enabled": map[string]string{"type": "boolean"}, "include_replies": map[string]string{"type": "boolean"}}}
		case "GET /api/v1/tasks":
			responseSchema = apiPageSchema("Task")
		case "GET /api/v1/tasks/{task}":
			responseSchema = apiRef("Task")
		case "POST /api/v1/accounts/{account}/tasks", "POST /api/v1/tasks/{task}/retry":
			success = "202"
			responseSchema = apiRef("Task")
		case "GET /api/v1/posts/{post}":
			responseSchema = apiRef("Post")
		case "GET /api/v1/accounts/{account}/timeline":
			responseSchema = apiPageSchema("Post")
		}
		responses[success] = apiJSONResponse("成功", responseSchema)
		if strings.HasSuffix(path, "/cancel") {
			responses["202"] = apiJSONResponse("停止请求已受理，查询任务直到结束。", map[string]string{"type": "object"})
		}
		if route.Scope == "media:read" {
			addQuery("v", map[string]string{"type": "string"})
			parameters = append(parameters, map[string]any{"name": "Range", "in": "header", "schema": map[string]string{"type": "string"}, "example": "bytes=0-1023"})
			for _, code := range []string{"200", "206"} {
				responses[code] = map[string]any{"description": "媒体二进制内容；必须每次携带凭据。", "content": map[string]any{"application/octet-stream": map[string]any{"schema": map[string]string{"type": "string", "format": "binary"}}}}
			}
			responses["416"] = map[string]string{"description": "请求范围不可满足"}
		}
		op := map[string]any{"summary": route.Summary, "description": "所需权限：" + route.Scope, "x-scope": route.Scope, "parameters": parameters, "responses": responses, "security": []any{map[string]any{"BearerAuth": []string{}}}}
		var body map[string]any
		var example any
		switch route.Pattern {
		case "POST /api/v1/accounts", "PATCH /api/v1/accounts/{account}":
			props := map[string]any{"enabled": map[string]string{"type": "boolean"}, "include_replies": map[string]string{"type": "boolean"}}
			body = map[string]any{"type": "object", "additionalProperties": false, "properties": props}
			if method == "POST" {
				props["username"] = map[string]string{"type": "string"}
				body["required"] = []string{"username"}
				example = map[string]any{"username": "example", "enabled": false, "include_replies": false}
			} else {
				body["minProperties"] = 1
				example = map[string]any{"enabled": true}
			}
		case "POST /api/v1/accounts/{account}/tasks":
			body = map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"mode": map[string]any{"type": "string", "enum": []string{"incremental", "backfill", "rescan"}, "default": "incremental"}}}
			example = map[string]string{"mode": "incremental"}
		}
		if body != nil {
			op["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": body, "example": example}}}
		}
		if paths[path] == nil {
			paths[path] = map[string]any{}
		}
		paths[path].(map[string]any)[strings.ToLower(method)] = op
	}
	post := apiSchema(PostView{})
	props := post["properties"].(map[string]any)
	props["media"] = map[string]any{"type": "array", "items": apiRef("Media")}
	props["quote"] = apiRef("Post")
	return map[string]any{"openapi": "3.0.3", "info": map[string]string{"title": "XM 外部 API", "version": "1.0.0", "description": "管理员签发凭据；共享授权归档。账号 id 为稳定归档键，任务 id 为不可复用的随机标识。"}, "servers": []any{map[string]string{"url": "/"}}, "paths": paths, "components": map[string]any{"securitySchemes": map[string]any{"BearerAuth": map[string]string{"type": "http", "scheme": "bearer"}}, "schemas": map[string]any{"Credential": apiSchema(apiCredential{}), "Account": apiSchema(externalAccountView{}), "Task": apiSchema(externalJobView{}), "Post": post, "Media": apiSchema(MediaView{}), "Error": map[string]any{"type": "object", "properties": map[string]any{"error": map[string]any{"type": "object", "properties": map[string]any{"code": map[string]string{"type": "string"}, "message": map[string]string{"type": "string"}, "request_id": map[string]string{"type": "string"}}}}}}}}
}
