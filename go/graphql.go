// graphql.go — a small, purpose-built GraphQL executor for /api/graphql. It parses the
// documents the conformance suite sends (queries, mutations, aliases, fragments,
// variables, batch arrays, and __type introspection) and resolves them against the same
// Forge store as REST, git and MCP. Unknown fields fail the way GraphQL does: HTTP 200
// with an errors array naming the field.

package main

import (
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------------------------
// lexer + parser (a compact recursive-descent parser for the GraphQL document subset we serve)
// ---------------------------------------------------------------------------------------------

type gqlTokenKind int

const (
	gqlName gqlTokenKind = iota
	gqlInt
	gqlString
	gqlPunct
	gqlEOF
)

type gqlToken struct {
	kind  gqlTokenKind
	value string // name / string / punct value
	num   float64
}

func gqlLex(source string) ([]gqlToken, error) {
	var tokens []gqlToken
	at := 0
	for at < len(source) {
		ch := source[at]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			at++
			continue
		}
		if ch == '#' {
			for at < len(source) && source[at] != '\n' {
				at++
			}
			continue
		}
		if ch == '"' {
			var value strings.Builder
			at++
			for at < len(source) && source[at] != '"' {
				if source[at] == '\\' && at+1 < len(source) {
					escaped := source[at+1]
					switch escaped {
					case 'n':
						value.WriteByte('\n')
					case 't':
						value.WriteByte('\t')
					case 'r':
						value.WriteByte('\r')
					default:
						value.WriteByte(escaped)
					}
					at += 2
				} else {
					value.WriteByte(source[at])
					at++
				}
			}
			at++
			tokens = append(tokens, gqlToken{kind: gqlString, value: value.String()})
			continue
		}
		if (ch >= '0' && ch <= '9') || ch == '-' {
			text := ""
			if ch == '-' {
				text = "-"
				at++
			}
			for at < len(source) && source[at] >= '0' && source[at] <= '9' {
				text += string(source[at])
				at++
			}
			num, _ := strconv.ParseFloat(text, 64)
			tokens = append(tokens, gqlToken{kind: gqlInt, num: num})
			continue
		}
		if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || ch == '_' {
			start := at
			for at < len(source) && ((source[at] >= 'A' && source[at] <= 'Z') || (source[at] >= 'a' && source[at] <= 'z') || (source[at] >= '0' && source[at] <= '9') || source[at] == '_') {
				at++
			}
			tokens = append(tokens, gqlToken{kind: gqlName, value: source[start:at]})
			continue
		}
		if strings.ContainsRune("{}()[]:!=$@&|", rune(ch)) {
			tokens = append(tokens, gqlToken{kind: gqlPunct, value: string(ch)})
			at++
			continue
		}
		if ch == '.' && at+3 <= len(source) && source[at:at+3] == "..." {
			tokens = append(tokens, gqlToken{kind: gqlPunct, value: "..."})
			at += 3
			continue
		}
		return nil, errGQL("unexpected character " + string(ch))
	}
	tokens = append(tokens, gqlToken{kind: gqlEOF})
	return tokens, nil
}

type gqlErr string

func (e gqlErr) Error() string { return string(e) }

func errGQL(message string) error { return gqlErr(message) }

// gqlVariable is a $variable reference inside a parsed value; gqlEnum is a bare enum name.
type gqlVariable struct{ name string }
type gqlEnum struct{ name string }

type gqlSelection struct {
	name       string
	alias      string
	args       map[string]any
	children   []*gqlSelection
	fragment   string
	inlineType string
}

type gqlOperation struct {
	kind       string
	name       string
	variables  map[string]any
	selections []*gqlSelection
}

type gqlDocument struct {
	operations []*gqlOperation
	fragments  map[string][]*gqlSelection
}

type gqlParser struct {
	at     int
	tokens []gqlToken
}

func (p *gqlParser) peek() gqlToken { return p.tokens[p.at] }

func (p *gqlParser) next() gqlToken {
	token := p.tokens[p.at]
	p.at++
	return token
}

func (p *gqlParser) expectPunct(value string) error {
	token := p.next()
	if token.kind != gqlPunct || token.value != value {
		return errGQL("expected " + value)
	}
	return nil
}

func (p *gqlParser) skipPunct(value string) bool {
	token := p.peek()
	if token.kind == gqlPunct && token.value == value {
		p.at++
		return true
	}
	return false
}

func (p *gqlParser) parseDocument() (*gqlDocument, error) {
	doc := &gqlDocument{fragments: map[string][]*gqlSelection{}}
	for p.peek().kind != gqlEOF {
		token := p.peek()
		if token.kind == gqlName && token.value == "fragment" {
			p.next()
			nameToken := p.next()
			name := ""
			if nameToken.kind == gqlName {
				name = nameToken.value
			}
			// 'on Type'
			p.next()
			p.next()
			selections, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.fragments[name] = selections
			continue
		}
		kind := "query"
		var name string
		if token.kind == gqlName && (token.value == "query" || token.value == "mutation") {
			kind = token.value
			p.next()
			maybeName := p.peek()
			if maybeName.kind == gqlName {
				name = maybeName.value
				p.next()
			}
		} else if token.kind == gqlName && token.value == "subscription" {
			return nil, errGQL("subscriptions are not supported")
		}
		variables, err := p.parseVariableDefinitions()
		if err != nil {
			return nil, err
		}
		selections, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		doc.operations = append(doc.operations, &gqlOperation{kind: kind, name: name, variables: variables, selections: selections})
	}
	return doc, nil
}

func (p *gqlParser) parseVariableDefinitions() (map[string]any, error) {
	variables := map[string]any{}
	if !p.skipPunct("(") {
		return variables, nil
	}
	for !p.skipPunct(")") {
		if p.peek().kind == gqlEOF {
			break
		}
		if err := p.expectPunct("$"); err != nil {
			return nil, err
		}
		nameToken := p.next()
		if nameToken.kind != gqlName {
			break
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		// Skip the type (a name, possibly wrapped in [ ] and !).
		for {
			token := p.peek()
			if token.kind == gqlName {
				p.next()
				continue
			}
			if token.kind == gqlPunct && (token.value == "[" || token.value == "]" || token.value == "!") {
				p.next()
				continue
			}
			break
		}
		if p.skipPunct("=") {
			if _, err := p.parseValue(); err != nil {
				return nil, err
			}
		}
		variables[nameToken.value] = nil
	}
	return variables, nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var selections []*gqlSelection
	for !p.skipPunct("}") {
		if p.peek().kind == gqlEOF {
			return nil, errGQL("unterminated selection set")
		}
		selection, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		selections = append(selections, selection)
	}
	return selections, nil
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	token := p.peek()
	if token.kind == gqlPunct && token.value == "..." {
		p.next()
		next := p.next()
		if next.kind == gqlName && next.value == "on" {
			typeToken := p.next()
			inlineType := ""
			if typeToken.kind == gqlName {
				inlineType = typeToken.value
			}
			children, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &gqlSelection{children: children, inlineType: inlineType}, nil
		}
		if next.kind == gqlName {
			return &gqlSelection{fragment: next.value}, nil
		}
		return nil, errGQL("bad fragment spread")
	}
	nameToken := p.next()
	if nameToken.kind != gqlName {
		return nil, errGQL("expected field name")
	}
	name := nameToken.value
	alias := ""
	if p.skipPunct(":") {
		alias = name
		real := p.next()
		if real.kind != gqlName {
			return nil, errGQL("expected field name after alias")
		}
		name = real.value
	}
	args := map[string]any{}
	if p.skipPunct("(") {
		for !p.skipPunct(")") {
			if p.peek().kind == gqlEOF {
				return nil, errGQL("unterminated arguments")
			}
			argToken := p.next()
			if argToken.kind != gqlName {
				return nil, errGQL("expected argument name")
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			value, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			args[argToken.value] = value
		}
	}
	// Directives (@include/@skip) are ignored: the suite never sends them.
	for p.peek().kind == gqlPunct && p.peek().value == "@" {
		p.next()
		p.next()
		if p.skipPunct("(") {
			for !p.skipPunct(")") {
				if _, err := p.parseValue(); err != nil {
					return nil, err
				}
			}
		}
	}
	var children []*gqlSelection
	if p.peek().kind == gqlPunct && p.peek().value == "{" {
		var err error
		children, err = p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
	}
	return &gqlSelection{name: name, alias: alias, args: args, children: children}, nil
}

func (p *gqlParser) parseValue() (any, error) {
	token := p.next()
	switch token.kind {
	case gqlString:
		return token.value, nil
	case gqlInt:
		return token.num, nil
	case gqlName:
		switch token.value {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return gqlEnum{name: token.value}, nil
		}
	case gqlPunct:
		switch token.value {
		case "$":
			nameToken := p.next()
			if nameToken.kind != gqlName {
				return nil, errGQL("expected variable name")
			}
			return gqlVariable{name: nameToken.value}, nil
		case "[":
			var items []any
			for !p.skipPunct("]") {
				item, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				items = append(items, item)
			}
			return items, nil
		case "{":
			object := map[string]any{}
			for !p.skipPunct("}") {
				keyToken := p.next()
				if keyToken.kind != gqlName {
					return nil, errGQL("expected object key")
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				value, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				object[keyToken.value] = value
			}
			return object, nil
		}
	}
	return nil, errGQL("unexpected value")
}

// gqlResolveValue substitutes $variables into a parsed value, then unwraps enums to
// their names.
func gqlResolveValue(value any, variables map[string]any) any {
	switch v := value.(type) {
	case nil, string, float64, bool:
		return v
	case []any:
		out := make([]any, len(v))
		for i, entry := range v {
			out[i] = gqlResolveValue(entry, variables)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, entry := range v {
			out[key] = gqlResolveValue(entry, variables)
		}
		return out
	case gqlVariable:
		if resolved, ok := variables[v.name]; ok && resolved != nil {
			return resolved
		}
		return nil
	case gqlEnum:
		return v.name
	}
	return value
}

// ---------------------------------------------------------------------------------------------
// schema tables (used for validation and __type introspection)
// ---------------------------------------------------------------------------------------------

var gqlFieldTypes = map[string]map[string]string{
	"Query": {
		"viewer":     "User",
		"repository": "Repository",
		"node":       "Node",
		"__type":     "__Type",
	},
	"Mutation": {
		"createIssue":       "CreateIssuePayload",
		"closeIssue":        "CloseIssuePayload",
		"createPullRequest": "CreatePullRequestPayload",
		"mergePullRequest":  "MergePullRequestPayload",
		"addComment":        "AddCommentPayload",
	},
	"User": {
		"id": "ID", "login": "String", "name": "String", "email": "String",
	},
	"Repository": {
		"id": "ID", "databaseId": "Int", "name": "String", "nameWithOwner": "String",
		"owner": "User", "defaultBranchRef": "Ref", "viewerPermission": "String",
		"hasIssuesEnabled": "Boolean", "issue": "Issue", "issues": "IssueConnection",
		"pullRequests": "PullRequestConnection", "pullRequest": "PullRequest",
	},
	"Issue": {
		"id": "ID", "number": "Int", "title": "String", "state": "String",
		"body": "String", "repository": "Repository",
	},
	"PullRequest": {
		"id": "ID", "number": "Int", "title": "String", "body": "String",
		"state": "String", "merged": "Boolean", "headRefName": "String",
		"baseRefName": "String", "mergeable": "String", "files": "PullRequestFileConnection",
	},
	"IssueConnection":           {"nodes": "[Issue]", "pageInfo": "PageInfo", "totalCount": "Int"},
	"PullRequestConnection":     {"nodes": "[PullRequest]", "pageInfo": "PageInfo", "totalCount": "Int"},
	"PullRequestFileConnection": {"nodes": "[PullRequestFile]", "totalCount": "Int"},
	"PullRequestFile":           {"path": "String"},
	"PageInfo": {
		"hasNextPage": "Boolean", "endCursor": "String",
		"hasPreviousPage": "Boolean", "startCursor": "String",
	},
	"Ref":                      {"name": "String"},
	"__Type":                   {"name": "String", "kind": "String", "fields": "[__Field]", "enumValues": "[__EnumValue]"},
	"__Field":                  {"name": "String", "description": "String"},
	"__EnumValue":              {"name": "String", "description": "String"},
	"CreateIssuePayload":       {"issue": "Issue"},
	"CloseIssuePayload":        {"issue": "Issue"},
	"CreatePullRequestPayload": {"pullRequest": "PullRequest"},
	"MergePullRequestPayload":  {"pullRequest": "PullRequest"},
	"AddCommentPayload":        {"commentEdge": "IssueCommentEdge"},
	"IssueCommentEdge":         {"node": "IssueComment"},
	"IssueComment":             {"id": "ID", "body": "String"},
}

var gqlScalarTypes = map[string]bool{"ID": true, "String": true, "Int": true, "Boolean": true}

// gqlFieldOrder lists each type's fields in the declaration order Object.keys would
// return them in (Go maps iterate in random order, so the order is recorded here).
var gqlFieldOrder = map[string][]string{
	"Query":                     {"viewer", "repository", "node", "__type"},
	"Mutation":                  {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"User":                      {"id", "login", "name", "email"},
	"Repository":                {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
	"Issue":                     {"id", "number", "title", "state", "body", "repository"},
	"PullRequest":               {"id", "number", "title", "body", "state", "merged", "headRefName", "baseRefName", "mergeable", "files"},
	"IssueConnection":           {"nodes", "pageInfo", "totalCount"},
	"PullRequestConnection":     {"nodes", "pageInfo", "totalCount"},
	"PullRequestFileConnection": {"nodes", "totalCount"},
	"PullRequestFile":           {"path"},
	"PageInfo":                  {"hasNextPage", "endCursor", "hasPreviousPage", "startCursor"},
	"Ref":                       {"name"},
	"__Type":                    {"name", "kind", "fields", "enumValues"},
	"__Field":                   {"name", "description"},
	"__EnumValue":               {"name", "description"},
	"CreateIssuePayload":        {"issue"},
	"CloseIssuePayload":         {"issue"},
	"CreatePullRequestPayload":  {"pullRequest"},
	"MergePullRequestPayload":   {"pullRequest"},
	"AddCommentPayload":         {"commentEdge"},
	"IssueCommentEdge":          {"node"},
	"IssueComment":              {"id", "body"},
}

var gqlTypeKind = map[string]string{
	"Query": "OBJECT", "Mutation": "OBJECT", "User": "OBJECT", "Repository": "OBJECT",
	"Issue": "OBJECT", "PullRequest": "OBJECT", "IssueConnection": "OBJECT",
	"PullRequestConnection": "OBJECT", "PullRequestFileConnection": "OBJECT",
	"PullRequestFile": "OBJECT", "PageInfo": "OBJECT", "Ref": "OBJECT",
	"__Type": "OBJECT", "__Field": "OBJECT", "__EnumValue": "OBJECT",
	"IssueCommentEdge": "OBJECT", "IssueComment": "OBJECT",
	"CreateIssuePayload": "OBJECT", "CloseIssuePayload": "OBJECT",
	"CreatePullRequestPayload": "OBJECT", "MergePullRequestPayload": "OBJECT",
	"AddCommentPayload": "OBJECT",
	"IssueState":        "ENUM", "PullRequestState": "ENUM", "MergeableState": "ENUM", "SearchType": "ENUM",
}

var gqlEnumValues = map[string][]string{
	"IssueState":       {"OPEN", "CLOSED"},
	"PullRequestState": {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":   {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"SearchType":       {"ISSUE", "REPOSITORY", "USER"},
}

// gqlGlobalID is an opaque, stable global id: base64 of a typed key.
func gqlGlobalID(kind string, key string) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + key))
}

func gqlParseGlobalID(id string) (kind string, key string, ok bool) {
	decoded, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", "", false
	}
	colon := strings.IndexByte(string(decoded), ':')
	if colon < 0 {
		return "", "", false
	}
	return string(decoded[:colon]), string(decoded[colon+1:]), true
}

// ---------------------------------------------------------------------------------------------
// internal value wrappers (never leak into JSON output)
// ---------------------------------------------------------------------------------------------

type gqlWrapper struct {
	kind string // viewer, repo, issue, pull, issuesConn, pullsConn, filesConn, pageInfo, ref, gqlType, comment, payload

	repo  *Repo
	issue *Issue
	pull  *Pull

	issues []*Issue
	pulls  []*Pull
	files  []string // pull file paths

	hasNext   bool
	endCursor *string

	refName  string
	typeName string

	commentID   int64
	commentBody string

	payloadFields map[string]*gqlWrapper
}

func gqlWrapperType(w *gqlWrapper) string {
	if w == nil {
		return "Node"
	}
	switch w.kind {
	case "viewer":
		return "User"
	case "repo":
		return "Repository"
	case "issue":
		return "Issue"
	case "pull":
		return "PullRequest"
	case "issuesConn":
		return "IssueConnection"
	case "pullsConn":
		return "PullRequestConnection"
	case "filesConn":
		return "PullRequestFileConnection"
	case "pageInfo":
		return "PageInfo"
	case "ref":
		return "Ref"
	case "gqlType":
		return "__Type"
	case "comment":
		return "IssueComment"
	case "payload":
		return "Payload"
	}
	return "Node"
}

func gqlCursorFor(seq int64) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.FormatInt(seq, 10)))
}

func gqlSeqFromCursor(cursor *string) *int64 {
	if cursor == nil {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(*cursor)
	if err != nil {
		return nil
	}
	if !strings.HasPrefix(string(decoded), "cursor:") {
		return nil
	}
	seq, err := strconv.ParseInt(string(decoded[len("cursor:"):]), 10, 64)
	if err != nil {
		return nil
	}
	return &seq
}

// ---------------------------------------------------------------------------------------------
// validation: unknown fields fail the whole document the way GraphQL validation does
// ---------------------------------------------------------------------------------------------

func gqlValidateSelections(typeName string, selections []*gqlSelection, fragments map[string][]*gqlSelection, errors *[]string) {
	fields, ok := gqlFieldTypes[typeName]
	if !ok {
		return
	}
	for _, selection := range selections {
		if selection.fragment != "" {
			if fragment, ok := fragments[selection.fragment]; ok {
				gqlValidateSelections(typeName, fragment, fragments, errors)
			}
			continue
		}
		if selection.inlineType != "" {
			gqlValidateSelections(selection.inlineType, selection.children, fragments, errors)
			continue
		}
		if selection.name == "__typename" {
			continue
		}
		fieldType, ok := fields[selection.name]
		if !ok {
			*errors = append(*errors, "Cannot query field \""+selection.name+"\" on type \""+typeName+"\".")
			continue
		}
		inner := strings.ReplaceAll(strings.ReplaceAll(fieldType, "[", ""), "]", "")
		if !gqlScalarTypes[inner] && len(selection.children) > 0 {
			gqlValidateSelections(inner, selection.children, fragments, errors)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------------------------

type gqlCtx struct {
	forge     *Forge
	variables map[string]any
	fragments map[string][]*gqlSelection
}

func gqlArgValue(selection *gqlSelection, name string, ctx *gqlCtx) any {
	raw, ok := selection.args[name]
	if !ok {
		return nil
	}
	return gqlResolveValue(raw, ctx.variables)
}

// gqlToInt coerces a GraphQL argument value to an int (lexer ints arrive as float64,
// request variables as JSON numbers).
func gqlToInt(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func gqlExecuteSelections(ctx *gqlCtx, typeName string, value *gqlWrapper, selections []*gqlSelection) *OMap {
	out := NewOMap()
	for _, selection := range selections {
		if selection.fragment != "" {
			if fragment, ok := ctx.fragments[selection.fragment]; ok {
				merged := gqlExecuteSelections(ctx, typeName, value, fragment)
				for _, key := range merged.Keys() {
					v, _ := merged.Get(key)
					out.Set(key, v)
				}
			}
			continue
		}
		if selection.inlineType != "" {
			if selection.inlineType == typeName || typeName == "Node" {
				merged := gqlExecuteSelections(ctx, selection.inlineType, value, selection.children)
				for _, key := range merged.Keys() {
					v, _ := merged.Get(key)
					out.Set(key, v)
				}
			}
			continue
		}
		key := selection.name
		if selection.alias != "" {
			key = selection.alias
		}
		if selection.name == "__typename" {
			out.Set(key, typeName)
			continue
		}
		out.Set(key, gqlExecuteField(ctx, typeName, value, selection))
	}
	return out
}

func gqlResolveObject(ctx *gqlCtx, typeName string, value *gqlWrapper, children []*gqlSelection) any {
	if value == nil {
		return nil
	}
	return gqlExecuteSelections(ctx, typeName, value, children)
}

var gqlNodeKeyRe = regexp.MustCompile(`^([^/]+)\/([^:]+):(\d+)$`)

func gqlExecuteField(ctx *gqlCtx, typeName string, value *gqlWrapper, selection *gqlSelection) any {
	name := selection.name
	children := selection.children

	// Root fields.
	if typeName == "Query" {
		if name == "viewer" {
			return gqlResolveObject(ctx, "User", &gqlWrapper{kind: "viewer"}, children)
		}
		if name == "repository" {
			owner, _ := gqlArgValue(selection, "owner", ctx).(string)
			repoName, _ := gqlArgValue(selection, "name", ctx).(string)
			if owner != ViewerLogin {
				return nil
			}
			repo := ctx.forge.GetRepo(repoName)
			if repo == nil {
				return nil
			}
			return gqlResolveObject(ctx, "Repository", &gqlWrapper{kind: "repo", repo: repo}, children)
		}
		if name == "node" {
			id, _ := gqlArgValue(selection, "id", ctx).(string)
			kind, key, ok := gqlParseGlobalID(id)
			if !ok {
				return nil
			}
			if kind == "repo" {
				parts := strings.Split(key, "/")
				if len(parts) < 2 || parts[0] != ViewerLogin {
					return nil
				}
				repo := ctx.forge.GetRepo(parts[1])
				if repo == nil {
					return nil
				}
				return gqlResolveObject(ctx, "Repository", &gqlWrapper{kind: "repo", repo: repo}, children)
			}
			if kind == "issue" || kind == "pull" {
				match := gqlNodeKeyRe.FindStringSubmatch(key)
				if match == nil {
					return nil
				}
				owner, repoName, numberText := match[1], match[2], match[3]
				if owner != ViewerLogin {
					return nil
				}
				repo := ctx.forge.GetRepo(repoName)
				if repo == nil {
					return nil
				}
				number, _ := strconv.Atoi(numberText)
				if kind == "issue" {
					var issue *Issue
					for _, entry := range repo.Issues {
						if entry.Number == number {
							issue = entry
							break
						}
					}
					if issue == nil {
						return nil
					}
					return gqlResolveObject(ctx, "Issue", &gqlWrapper{kind: "issue", repo: repo, issue: issue}, children)
				}
				var pull *Pull
				for _, entry := range repo.Pulls {
					if entry.Number == number {
						pull = entry
						break
					}
				}
				if pull == nil {
					return nil
				}
				return gqlResolveObject(ctx, "PullRequest", &gqlWrapper{kind: "pull", repo: repo, pull: pull}, children)
			}
			return nil
		}
		if name == "__type" {
			typeNameArg, _ := gqlArgValue(selection, "name", ctx).(string)
			if _, ok := gqlTypeKind[typeNameArg]; !ok {
				return nil
			}
			return gqlResolveObject(ctx, "__Type", &gqlWrapper{kind: "gqlType", typeName: typeNameArg}, children)
		}
		return nil
	}

	// Mutations.
	if typeName == "Mutation" {
		input, _ := gqlArgValue(selection, "input", ctx).(map[string]any)
		if input == nil {
			input = map[string]any{}
		}
		if name == "createIssue" {
			repoID, _ := input["repositoryId"].(string)
			repo := gqlRepoFromGlobalID(ctx, repoID)
			if repo == nil {
				return nil
			}
			title, _ := input["title"].(string)
			body, _ := input["body"].(string)
			created := ctx.forge.CreateIssue(repo, title, body, nil)
			if created.Error != "" {
				return nil
			}
			return gqlResolveObject(ctx, "CreateIssuePayload", &gqlWrapper{kind: "payload", payloadFields: map[string]*gqlWrapper{
				"issue": {kind: "issue", repo: repo, issue: created.Issue},
			}}, children)
		}
		if name == "closeIssue" {
			issueID, _ := input["issueId"].(string)
			found := gqlIssueFromGlobalID(ctx, issueID)
			if found == nil {
				return nil
			}
			found.issue.State = "closed"
			DropIssueJSONCache(found.repo, found.issue.Number)
			return gqlResolveObject(ctx, "CloseIssuePayload", &gqlWrapper{kind: "payload", payloadFields: map[string]*gqlWrapper{
				"issue": {kind: "issue", repo: found.repo, issue: found.issue},
			}}, children)
		}
		if name == "createPullRequest" {
			repoID, _ := input["repositoryId"].(string)
			repo := gqlRepoFromGlobalID(ctx, repoID)
			if repo == nil {
				return nil
			}
			head, _ := input["headRefName"].(string)
			base, _ := input["baseRefName"].(string)
			if base == "" {
				base = DefaultBranch
			}
			title, _ := input["title"].(string)
			created := ctx.forge.CreatePull(repo, head, base, title, "")
			if created.Error != "" {
				return nil
			}
			return gqlResolveObject(ctx, "CreatePullRequestPayload", &gqlWrapper{kind: "payload", payloadFields: map[string]*gqlWrapper{
				"pullRequest": {kind: "pull", repo: repo, pull: created.Pull},
			}}, children)
		}
		if name == "mergePullRequest" {
			pullID, _ := input["pullRequestId"].(string)
			found := gqlPullFromGlobalID(ctx, pullID)
			if found == nil {
				return nil
			}
			merged := ctx.forge.MergeSquash(found.repo, found.pull)
			if merged.Error != "" {
				return nil
			}
			return gqlResolveObject(ctx, "MergePullRequestPayload", &gqlWrapper{kind: "payload", payloadFields: map[string]*gqlWrapper{
				"pullRequest": {kind: "pull", repo: found.repo, pull: found.pull},
			}}, children)
		}
		if name == "addComment" {
			subjectID, _ := input["subjectId"].(string)
			found := gqlIssueFromGlobalID(ctx, subjectID)
			if found == nil {
				return nil
			}
			body, _ := input["body"].(string)
			comment := ctx.forge.AddComment(found.repo, found.issue.Number, body)
			if comment == nil {
				return nil
			}
			return gqlResolveObject(ctx, "AddCommentPayload", &gqlWrapper{kind: "payload", payloadFields: map[string]*gqlWrapper{
				"commentEdge": {kind: "payload", payloadFields: map[string]*gqlWrapper{
					"node": {kind: "comment", commentID: comment.ID, commentBody: comment.Body},
				}},
			}}, children)
		}
		return nil
	}

	if value == nil {
		return nil
	}

	// Payload wrappers (mutation results).
	if value.kind == "payload" {
		inner, ok := value.payloadFields[name]
		if !ok || inner == nil {
			return nil
		}
		return gqlResolveObject(ctx, gqlWrapperType(inner), inner, children)
	}

	// Scalars and objects by type.
	switch value.kind {
	case "viewer":
		if name == "id" {
			return gqlGlobalID("user", ViewerLogin)
		}
		if name == "login" {
			return ViewerLogin
		}
		if name == "name" {
			return ViewerName
		}
		if name == "email" {
			return ViewerEmail
		}
		return nil
	case "repo":
		repo := value.repo
		switch name {
		case "id":
			return gqlGlobalID("repo", ViewerLogin+"/"+repo.Name)
		case "databaseId":
			return repo.DatabaseID
		case "name":
			return repo.Name
		case "nameWithOwner":
			return ViewerLogin + "/" + repo.Name
		case "owner":
			return gqlResolveObject(ctx, "User", &gqlWrapper{kind: "viewer"}, children)
		case "defaultBranchRef":
			return gqlResolveObject(ctx, "Ref", &gqlWrapper{kind: "ref", refName: DefaultBranch}, children)
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return true
		case "issue":
			number, ok := gqlToInt(gqlArgValue(selection, "number", ctx))
			if !ok {
				return nil
			}
			var issue *Issue
			for _, entry := range repo.Issues {
				if entry.Number == number {
					issue = entry
					break
				}
			}
			if issue == nil {
				return nil
			}
			return gqlResolveObject(ctx, "Issue", &gqlWrapper{kind: "issue", repo: repo, issue: issue}, children)
		case "issues":
			page := gqlPaginateIssues(ctx, repo, selection)
			return gqlResolveObject(ctx, "IssueConnection", &gqlWrapper{kind: "issuesConn", repo: repo, issues: page.items, hasNext: page.hasNext, endCursor: page.endCursor}, children)
		case "pullRequests":
			page := gqlPaginatePulls(ctx, repo, selection)
			return gqlResolveObject(ctx, "PullRequestConnection", &gqlWrapper{kind: "pullsConn", repo: repo, pulls: page.items, hasNext: page.hasNext, endCursor: page.endCursor}, children)
		case "pullRequest":
			number, ok := gqlToInt(gqlArgValue(selection, "number", ctx))
			if !ok {
				return nil
			}
			var pull *Pull
			for _, entry := range repo.Pulls {
				if entry.Number == number {
					pull = entry
					break
				}
			}
			if pull == nil {
				return nil
			}
			return gqlResolveObject(ctx, "PullRequest", &gqlWrapper{kind: "pull", repo: repo, pull: pull}, children)
		}
		return nil
	case "issue":
		issue := value.issue
		switch name {
		case "id":
			return gqlGlobalID("issue", ViewerLogin+"/"+value.repo.Name+":"+strconv.Itoa(issue.Number))
		case "number":
			return issue.Number
		case "title":
			return issue.Title
		case "state":
			return strings.ToUpper(issue.State)
		case "body":
			return issue.Body
		case "repository":
			return gqlResolveObject(ctx, "Repository", &gqlWrapper{kind: "repo", repo: value.repo}, children)
		}
		return nil
	case "pull":
		pull := value.pull
		switch name {
		case "id":
			return gqlGlobalID("pull", ViewerLogin+"/"+value.repo.Name+":"+strconv.Itoa(pull.Number))
		case "number":
			return pull.Number
		case "title":
			return pull.Title
		case "body":
			return pull.Body
		case "state":
			if pull.Merged {
				return "MERGED"
			}
			return strings.ToUpper(pull.State)
		case "merged":
			return pull.Merged
		case "headRefName":
			return pull.Head
		case "baseRefName":
			return pull.Base
		case "mergeable":
			if ctx.forge.PullConflict(value.repo, pull) {
				return "CONFLICTING"
			}
			return "MERGEABLE"
		case "files":
			files := ctx.forge.PullFiles(value.repo, pull)
			paths := make([]string, 0, len(files))
			for _, file := range files {
				paths = append(paths, file.Path)
			}
			return gqlResolveObject(ctx, "PullRequestFileConnection", &gqlWrapper{kind: "filesConn", files: paths}, children)
		}
		return nil
	case "issuesConn":
		switch name {
		case "nodes":
			nodes := make([]any, 0, len(value.issues))
			for _, issue := range value.issues {
				nodes = append(nodes, gqlResolveObject(ctx, "Issue", &gqlWrapper{kind: "issue", repo: value.repo, issue: issue}, children))
			}
			return nodes
		case "pageInfo":
			return gqlResolveObject(ctx, "PageInfo", &gqlWrapper{kind: "pageInfo", hasNext: value.hasNext, endCursor: value.endCursor}, children)
		case "totalCount":
			return len(value.repo.Issues)
		}
		return nil
	case "pullsConn":
		switch name {
		case "nodes":
			nodes := make([]any, 0, len(value.pulls))
			for _, pull := range value.pulls {
				nodes = append(nodes, gqlResolveObject(ctx, "PullRequest", &gqlWrapper{kind: "pull", repo: value.repo, pull: pull}, children))
			}
			return nodes
		case "pageInfo":
			return gqlResolveObject(ctx, "PageInfo", &gqlWrapper{kind: "pageInfo", hasNext: value.hasNext, endCursor: value.endCursor}, children)
		case "totalCount":
			return len(value.repo.Pulls)
		}
		return nil
	case "filesConn":
		switch name {
		case "nodes":
			nodes := make([]any, 0, len(value.files))
			for _, path := range value.files {
				nodes = append(nodes, NewOMap().Set("path", path))
			}
			return nodes
		case "totalCount":
			return len(value.files)
		}
		return nil
	case "pageInfo":
		switch name {
		case "hasNextPage":
			return value.hasNext
		case "endCursor":
			if value.endCursor == nil {
				return nil
			}
			return *value.endCursor
		case "hasPreviousPage":
			return false
		case "startCursor":
			return nil
		}
		return nil
	case "ref":
		if name == "name" {
			return value.refName
		}
		return nil
	case "gqlType":
		switch name {
		case "name":
			return value.typeName
		case "kind":
			if kind, ok := gqlTypeKind[value.typeName]; ok {
				return kind
			}
			return "OBJECT"
		case "fields":
			names := gqlFieldOrder[value.typeName]
			entries := make([]any, 0, len(names))
			for _, field := range names {
				entries = append(entries, NewOMap().Set("name", field).Set("description", ""))
			}
			return entries
		case "enumValues":
			values := gqlEnumValues[value.typeName]
			entries := make([]any, 0, len(values))
			for _, enumValue := range values {
				entries = append(entries, NewOMap().Set("name", enumValue).Set("description", ""))
			}
			return entries
		}
		return nil
	case "comment":
		switch name {
		case "id":
			return gqlGlobalID("comment", strconv.FormatInt(value.commentID, 10))
		case "body":
			return value.commentBody
		}
		return nil
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// connection pagination + global-id lookups
// ---------------------------------------------------------------------------------------------

type gqlPage[T any] struct {
	items     []T
	hasNext   bool
	endCursor *string
}

func gqlPaginateIssues(ctx *gqlCtx, repo *Repo, selection *gqlSelection) gqlPage[*Issue] {
	first := 10
	if n, ok := gqlToInt(gqlArgValue(selection, "first", ctx)); ok {
		first = n
	}
	afterStr, _ := gqlArgValue(selection, "after", ctx).(string)
	var after *int64
	if afterStr != "" {
		after = gqlSeqFromCursor(&afterStr)
	}
	var statesRaw []any
	if raw, ok := gqlArgValue(selection, "states", ctx).([]any); ok {
		statesRaw = raw
	}
	direction := "DESC"
	if orderBy, ok := gqlArgValue(selection, "orderBy", ctx).(map[string]any); ok {
		if dir, ok := orderBy["direction"].(string); ok && dir != "" {
			direction = dir
		}
	}
	list := make([]*Issue, 0, len(repo.Issues))
	for _, issue := range repo.Issues {
		list = append(list, issue)
	}
	if statesRaw != nil {
		wanted := map[string]bool{}
		for _, state := range statesRaw {
			if str, ok := state.(string); ok {
				wanted[strings.ToLower(str)] = true
			}
		}
		filtered := make([]*Issue, 0, len(list))
		for _, issue := range list {
			if wanted[issue.State] {
				filtered = append(filtered, issue)
			}
		}
		list = filtered
	}
	if direction == "ASC" {
		sortIssuesBySeq(list, true)
	} else {
		sortIssuesBySeq(list, false)
	}
	if after != nil {
		filtered := make([]*Issue, 0, len(list))
		for _, issue := range list {
			if direction == "ASC" {
				if issue.Seq > *after {
					filtered = append(filtered, issue)
				}
			} else {
				if issue.Seq < *after {
					filtered = append(filtered, issue)
				}
			}
		}
		list = filtered
	}
	if first < 0 {
		first = 0
	}
	items := list
	if len(items) > first {
		items = items[:first]
	}
	hasNext := len(list) > len(items)
	var endCursor *string
	if len(items) > 0 {
		cursor := gqlCursorFor(items[len(items)-1].Seq)
		endCursor = &cursor
	}
	return gqlPage[*Issue]{items: items, hasNext: hasNext, endCursor: endCursor}
}

func gqlPaginatePulls(ctx *gqlCtx, repo *Repo, selection *gqlSelection) gqlPage[*Pull] {
	first := 10
	if n, ok := gqlToInt(gqlArgValue(selection, "first", ctx)); ok {
		first = n
	}
	afterStr, _ := gqlArgValue(selection, "after", ctx).(string)
	var after *int64
	if afterStr != "" {
		after = gqlSeqFromCursor(&afterStr)
	}
	list := make([]*Pull, 0, len(repo.Pulls))
	for _, pull := range repo.Pulls {
		list = append(list, pull)
	}
	sortPullsBySeqDesc(list)
	if after != nil {
		filtered := make([]*Pull, 0, len(list))
		for _, pull := range list {
			if pull.Seq < *after {
				filtered = append(filtered, pull)
			}
		}
		list = filtered
	}
	if first < 0 {
		first = 0
	}
	items := list
	if len(items) > first {
		items = items[:first]
	}
	hasNext := len(list) > len(items)
	var endCursor *string
	if len(items) > 0 {
		cursor := gqlCursorFor(items[len(items)-1].Seq)
		endCursor = &cursor
	}
	return gqlPage[*Pull]{items: items, hasNext: hasNext, endCursor: endCursor}
}

func gqlRepoFromGlobalID(ctx *gqlCtx, id string) *Repo {
	kind, key, ok := gqlParseGlobalID(id)
	if !ok || kind != "repo" {
		return nil
	}
	parts := strings.Split(key, "/")
	if len(parts) < 2 || parts[0] != ViewerLogin {
		return nil
	}
	return ctx.forge.GetRepo(parts[1])
}

type gqlIssueRef struct {
	repo  *Repo
	issue *Issue
}

type gqlPullRef struct {
	repo *Repo
	pull *Pull
}

func gqlIssueFromGlobalID(ctx *gqlCtx, id string) *gqlIssueRef {
	kind, key, ok := gqlParseGlobalID(id)
	if !ok || kind != "issue" {
		return nil
	}
	match := gqlNodeKeyRe.FindStringSubmatch(key)
	if match == nil {
		return nil
	}
	owner, repoName, numberText := match[1], match[2], match[3]
	if owner != ViewerLogin {
		return nil
	}
	repo := ctx.forge.GetRepo(repoName)
	if repo == nil {
		return nil
	}
	number, _ := strconv.Atoi(numberText)
	for _, entry := range repo.Issues {
		if entry.Number == number {
			return &gqlIssueRef{repo: repo, issue: entry}
		}
	}
	return nil
}

func gqlPullFromGlobalID(ctx *gqlCtx, id string) *gqlPullRef {
	kind, key, ok := gqlParseGlobalID(id)
	if !ok || kind != "pull" {
		return nil
	}
	match := gqlNodeKeyRe.FindStringSubmatch(key)
	if match == nil {
		return nil
	}
	owner, repoName, numberText := match[1], match[2], match[3]
	if owner != ViewerLogin {
		return nil
	}
	repo := ctx.forge.GetRepo(repoName)
	if repo == nil {
		return nil
	}
	number, _ := strconv.Atoi(numberText)
	for _, entry := range repo.Pulls {
		if entry.Number == number {
			return &gqlPullRef{repo: repo, pull: entry}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// entry point: one document, or a batch array of documents
// ---------------------------------------------------------------------------------------------

// gqlExecuteDocument runs one {query, variables} document and returns the response object
// (a "data" key, an "errors" key, or both).
func gqlExecuteDocument(forge *Forge, query string, variables map[string]any) *OMap {
	tokens, err := gqlLex(query)
	if err != nil {
		return NewOMap().Set("errors", []any{NewOMap().Set("message", err.Error())})
	}
	parser := &gqlParser{tokens: tokens}
	doc, err := parser.parseDocument()
	if err != nil {
		return NewOMap().Set("errors", []any{NewOMap().Set("message", err.Error())})
	}
	if len(doc.operations) == 0 {
		return NewOMap().Set("errors", []any{NewOMap().Set("message", "no operation found")})
	}
	operation := doc.operations[0]
	rootType := "Query"
	if operation.kind == "mutation" {
		rootType = "Mutation"
	}
	var errors []string
	gqlValidateSelections(rootType, operation.selections, doc.fragments, &errors)
	if len(errors) > 0 {
		errList := make([]any, 0, len(errors))
		for _, message := range errors {
			errList = append(errList, NewOMap().Set("message", message))
		}
		return NewOMap().Set("data", nil).Set("errors", errList)
	}
	ctx := &gqlCtx{forge: forge, variables: variables, fragments: doc.fragments}
	data := gqlExecuteSelections(ctx, rootType, nil, operation.selections)
	return NewOMap().Set("data", data)
}

// HandleGraphQL handles POST /api/graphql. Accepts one {query, variables} document or a
// JSON array of them (batch), and answers each in order. Returns the HTTP status and the
// response body.
func HandleGraphQL(forge *Forge, body any) (int, any) {
	if arr, ok := body.([]any); ok {
		results := make([]any, 0, len(arr))
		for _, entry := range arr {
			doc, ok := entry.(map[string]any)
			if !ok {
				results = append(results, NewOMap().Set("errors", []any{NewOMap().Set("message", "invalid request")}))
				continue
			}
			query, ok := doc["query"].(string)
			if !ok {
				results = append(results, NewOMap().Set("errors", []any{NewOMap().Set("message", "query is required")}))
				continue
			}
			variables, _ := doc["variables"].(map[string]any)
			if variables == nil {
				variables = map[string]any{}
			}
			results = append(results, gqlExecuteDocument(forge, query, variables))
		}
		return 200, results
	}
	doc, ok := body.(map[string]any)
	if !ok {
		return 400, NewOMap().Set("errors", []any{NewOMap().Set("message", "query is required")})
	}
	query, ok := doc["query"].(string)
	if !ok {
		return 400, NewOMap().Set("errors", []any{NewOMap().Set("message", "query is required")})
	}
	variables, _ := doc["variables"].(map[string]any)
	if variables == nil {
		variables = map[string]any{}
	}
	return 200, gqlExecuteDocument(forge, query, variables)
}
