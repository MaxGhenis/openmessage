package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
)

func listContactsTool() mcp.Tool {
	return mcp.NewTool("list_contacts",
		mcp.WithDescription("List or search contacts by name or phone number"),
		mcp.WithString("query", mcp.Description("Search by name or number")),
		mcp.WithNumber("limit", mcp.Description("Maximum contacts to return (default 50)")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

func listContactsHandler(a *app.App) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		query := strArg(args, "query")
		limit := intArg(args, "limit", 50)

		// If no contacts in DB yet, try fetching from phone
		contacts, err := a.Store.ListContacts("", 1)
		if err == nil && len(contacts) == 0 && a.Client != nil {
			if err := fetchAndCacheContacts(a); err != nil {
				a.Logger.Warn().Err(err).Msg("Failed to fetch contacts from phone")
			}
		}

		contacts, err = a.Store.ListContacts(query, limit)
		if err != nil {
			return errorResult(fmt.Sprintf("query failed: %v", err)), nil
		}

		// Fall back to conversation participants if contacts table is empty
		if len(contacts) == 0 {
			contacts, err = a.Store.ListContactsFromConversations(query, limit)
			if err != nil {
				return errorResult(fmt.Sprintf("query failed: %v", err)), nil
			}
		}

		if len(contacts) == 0 {
			return textResult("No contacts found."), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "%d contacts:\n\n", len(contacts))
		for _, c := range contacts {
			if c.Number != "" {
				fmt.Fprintf(&sb, "- %s: %s\n", c.Name, c.Number)
			} else {
				fmt.Fprintf(&sb, "- %s\n", c.Name)
			}
		}
		return textResult(sb.String()), nil
	}
}

func fetchAndCacheContacts(a *app.App) error {
	if a.Client == nil {
		return fmt.Errorf("not connected")
	}
	resp, err := a.Client.GM.ListContacts()
	if err != nil {
		return err
	}
	for _, c := range resp.GetContacts() {
		number := ""
		if n := c.GetNumber(); n != nil {
			number = n.GetNumber()
		}
		contact := &db.Contact{
			ContactID: c.GetContactID(),
			Name:      c.GetName(),
			Number:    number,
		}
		if err := a.Store.UpsertContact(contact); err != nil {
			a.Logger.Warn().Err(err).Str("id", contact.ContactID).Msg("Failed to cache contact")
		}
	}
	return nil
}
