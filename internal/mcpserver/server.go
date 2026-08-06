// Package mcpserver exposes the knowledge bases as MCP tools over streamable
// HTTP (stateless), the primary interface for coding agents and the andino
// agent-runtime.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/andino-agents/knowledge-base/internal/app"
	"github.com/andino-agents/knowledge-base/internal/store"
)

// New builds the MCP server with every tool registered.
func New(a *app.App, version string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "andino-kb",
		Title:   "Andino Knowledge Base",
		Version: version,
	}, nil)

	type searchArgs struct {
		Query         string  `json:"query" jsonschema:"the search query, natural language or keywords"`
		KnowledgeBase string  `json:"knowledge_base,omitempty" jsonschema:"restrict the search to one knowledge base; omit to search all"`
		Limit         int     `json:"limit,omitempty" jsonschema:"maximum results, default 8"`
		MinScore      float64 `json:"min_score,omitempty" jsonschema:"minimum normalized relevance 0..1, default 0"`
		Rerank        *bool   `json:"rerank,omitempty" jsonschema:"override reranking for this query; omit for the knowledge base default"`
		MaxPerDoc     int     `json:"max_per_doc,omitempty" jsonschema:"max chunks from the same document; 0 = default (2), -1 = unlimited"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "search",
		Description: "Hybrid (keyword + semantic) search across the knowledge bases. " +
			"Use this first to find relevant content; every result carries its document path " +
			"and line span so you can expand context with get_document.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(args.Query) == "" {
			return nil, nil, fmt.Errorf("query is required")
		}
		var (
			hits []app.Hit
			err  error
		)
		opts := app.SearchOpts{Limit: args.Limit, MinScore: args.MinScore, Rerank: args.Rerank, MaxPerDoc: args.MaxPerDoc}
		if args.KnowledgeBase != "" {
			hits, err = a.Search(ctx, args.KnowledgeBase, args.Query, opts)
		} else {
			hits, err = a.SearchAll(ctx, args.Query, opts)
		}
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"results": hits})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_knowledge_bases",
		Description: "List the available knowledge bases with document counts and whether they accept writes.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, any, error) {
		type info struct {
			Name        string `json:"name"`
			Description string `json:"description,omitempty"`
			Writable    bool   `json:"writable"`
			Documents   int64  `json:"documents"`
			Chunks      int64  `json:"chunks"`
		}
		var out []info
		for _, name := range a.KBNames() {
			kb, err := a.KB(name)
			if err != nil {
				continue
			}
			st, err := kb.Store.Stats(ctx)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, info{
				Name: name, Description: kb.Config.Description,
				Writable: kb.Config.Writable, Documents: st.Documents, Chunks: st.Chunks,
			})
		}
		return jsonResult(map[string]any{"knowledge_bases": out})
	})

	type listDocsArgs struct {
		KnowledgeBase string `json:"knowledge_base" jsonschema:"the knowledge base to list"`
		Prefix        string `json:"prefix,omitempty" jsonschema:"filter documents whose path starts with this prefix"`
		Cursor        string `json:"cursor,omitempty" jsonschema:"pagination cursor from a previous call"`
		Limit         int    `json:"limit,omitempty" jsonschema:"page size, default 50"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_documents",
		Description: "Page through the documents of a knowledge base.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args listDocsArgs) (*mcp.CallToolResult, any, error) {
		kb, err := a.KB(args.KnowledgeBase)
		if err != nil {
			return nil, nil, err
		}
		docs, err := kb.Store.ListDocuments(ctx, args.Prefix, args.Cursor, args.Limit)
		if err != nil {
			return nil, nil, err
		}
		next := ""
		if len(docs) > 0 {
			next = docs[len(docs)-1].RelPath
		}
		return jsonResult(map[string]any{"documents": docs, "next_cursor": next})
	})

	type getDocArgs struct {
		KnowledgeBase string `json:"knowledge_base" jsonschema:"the knowledge base the document lives in"`
		Path          string `json:"path" jsonschema:"the document's rel_path (or memory id) as returned by search or list_documents"`
		StartLine     int    `json:"start_line,omitempty" jsonschema:"first line to return (1-based); omit for the whole document"`
		EndLine       int    `json:"end_line,omitempty" jsonschema:"last line to return (inclusive)"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_document",
		Description: "Fetch a document's content, optionally sliced by lines. " +
			"Use it to expand the context around a search hit.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getDocArgs) (*mcp.CallToolResult, any, error) {
		kb, err := a.KB(args.KnowledgeBase)
		if err != nil {
			return nil, nil, err
		}
		doc, err := kb.Store.GetDocument(ctx, "", args.Path)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, fmt.Errorf("document %q not found in %s", args.Path, args.KnowledgeBase)
		}
		if err != nil {
			return nil, nil, err
		}
		text := doc.Text
		if args.StartLine > 0 || args.EndLine > 0 {
			text = sliceLines(text, args.StartLine, args.EndLine)
		}
		return jsonResult(map[string]any{"document": doc.Document, "content": text})
	})

	type storeArgs struct {
		KnowledgeBase string `json:"knowledge_base" jsonschema:"a writable knowledge base (see list_knowledge_bases)"`
		Content       string `json:"content" jsonschema:"the content to remember, markdown welcome"`
		Title         string `json:"title,omitempty" jsonschema:"optional short title"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name: "store",
		Description: "Persist content into a writable knowledge base so any agent can retrieve it later " +
			"with search. Returns the document id.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args storeArgs) (*mcp.CallToolResult, any, error) {
		id, err := a.StoreDocument(ctx, args.KnowledgeBase, args.Title, args.Content)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"document_id": id, "status": "stored"})
	})

	type deleteArgs struct {
		KnowledgeBase string `json:"knowledge_base" jsonschema:"the writable knowledge base"`
		DocumentID    string `json:"document_id" jsonschema:"the id returned by store"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_document",
		Description: "Delete a stored document from a writable knowledge base by its id.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args deleteArgs) (*mcp.CallToolResult, any, error) {
		if err := a.DeleteDocument(ctx, args.KnowledgeBase, args.DocumentID); err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"status": "deleted"})
	})

	return srv
}

func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}

func sliceLines(text string, start, end int) string {
	lines := strings.Split(text, "\n")
	if start < 1 {
		start = 1
	}
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if start > len(lines) || start > end {
		return ""
	}
	return strings.Join(lines[start-1:end], "\n")
}
