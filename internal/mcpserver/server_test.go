package mcpserver

import (
	"context"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolNames connects a client to the server over an in-memory transport and
// returns what tools/list advertises. The *app.App is nil on purpose: New only
// registers handlers, it never dereferences the app, and listing tools does
// not invoke them.
func toolNames(t *testing.T, allowWrites bool) []string {
	t.Helper()
	ctx := context.Background()
	clientTr, serverTr := mcp.NewInMemoryTransports()

	ss, err := New(nil, "test", allowWrites).Connect(ctx, serverTr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil).Connect(ctx, clientTr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

// A read-scoped key must not even see the write tools. This is the regression
// test for the scope bypass: before the fix, /mcp handed every valid key the
// full tool set regardless of scope.
func TestToolsByScope(t *testing.T) {
	readOnly := []string{"get_document", "list_documents", "list_knowledge_bases", "search"}
	readWrite := []string{"delete_document", "get_document", "list_documents", "list_knowledge_bases", "search", "store"}

	if got := toolNames(t, false); !slices.Equal(got, readOnly) {
		t.Errorf("read-only tools = %v, want %v", got, readOnly)
	}
	if got := toolNames(t, true); !slices.Equal(got, readWrite) {
		t.Errorf("read-write tools = %v, want %v", got, readWrite)
	}
}
