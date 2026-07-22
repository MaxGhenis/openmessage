package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

type platformStatSummary struct {
	Platform         string `json:"platform"`
	Count            int    `json:"count"`
	LatestMS         int64  `json:"latest_ms"`
	LatestReceivedMS int64  `json:"latest_received_ms"`
}

var (
	googleStatus = func(a *app.App) app.GoogleStatusSnapshot {
		return a.GoogleStatus()
	}
	whatsAppStatus = func(a *app.App) whatsapplive.StatusSnapshot {
		return a.WhatsAppStatus()
	}
	signalStatus = func(a *app.App) signallive.StatusSnapshot {
		return a.SignalStatus()
	}
)

func getStatusTool() mcp.Tool {
	return mcp.NewTool("get_status",
		mcp.WithDescription("Get connection and pairing status for Google Messages, WhatsApp, and Signal"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

func getStatusHandler(a *app.App, configured ...Options) server.ToolHandlerFunc {
	options := resolvedOptions(a, configured)
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var sb strings.Builder
		var storedPlatforms []platformStatSummary
		if options.V2Primary {
			stats, err := options.Reads.PlatformStats()
			if err != nil {
				return errorResult(fmt.Sprintf("query serving store status: %v", err)), nil
			}
			storedPlatforms = make([]platformStatSummary, 0, len(stats))
			for _, stat := range stats {
				storedPlatforms = append(storedPlatforms, platformStatSummary{
					Platform:         stat.Platform,
					Count:            stat.Count,
					LatestMS:         stat.LatestMS,
					LatestReceivedMS: stat.LatestRecvMS,
				})
			}
		}

		google := googleStatus(a)
		whatsApp := whatsAppStatus(a)
		signal := signalStatus(a)

		overallConnected := google.Connected || whatsApp.Connected || signal.Connected
		sb.WriteString("Overall: ")
		if overallConnected {
			sb.WriteString("connected\n")
		} else {
			sb.WriteString("not connected\n")
		}

		sb.WriteString("\nGoogle Messages:\n")
		fmt.Fprintf(&sb, "  Connected: %v\n", google.Connected)
		fmt.Fprintf(&sb, "  Paired: %v\n", google.Paired)
		fmt.Fprintf(&sb, "  Needs pairing: %v\n", google.NeedsPairing)
		fmt.Fprintf(&sb, "  Phone responding: %v\n", google.PhoneResponding)
		if google.RepairsPaced > 0 {
			fmt.Fprintf(&sb, "  Repairs paced: %d (cookie repairs delayed by the min-interval floor)\n", google.RepairsPaced)
		}
		if google.LastError != "" {
			fmt.Fprintf(&sb, "  Last error: %s\n", google.LastError)
		}

		sb.WriteString("\nWhatsApp:\n")
		fmt.Fprintf(&sb, "  Connected: %v\n", whatsApp.Connected)
		fmt.Fprintf(&sb, "  Connecting: %v\n", whatsApp.Connecting)
		fmt.Fprintf(&sb, "  Paired: %v\n", whatsApp.Paired)
		fmt.Fprintf(&sb, "  Pairing: %v\n", whatsApp.Pairing)
		fmt.Fprintf(&sb, "  QR available: %v\n", whatsApp.QRAvailable)
		if whatsApp.AccountJID != "" {
			fmt.Fprintf(&sb, "  Account: %s\n", whatsApp.AccountJID)
		}
		if whatsApp.PushName != "" {
			fmt.Fprintf(&sb, "  Push name: %s\n", whatsApp.PushName)
		}
		if whatsApp.LastError != "" {
			fmt.Fprintf(&sb, "  Last error: %s\n", whatsApp.LastError)
		}

		sb.WriteString("\nSignal:\n")
		fmt.Fprintf(&sb, "  Connected: %v\n", signal.Connected)
		fmt.Fprintf(&sb, "  Connecting: %v\n", signal.Connecting)
		fmt.Fprintf(&sb, "  Paired: %v\n", signal.Paired)
		fmt.Fprintf(&sb, "  Pairing: %v\n", signal.Pairing)
		fmt.Fprintf(&sb, "  QR available: %v\n", signal.QRAvailable)
		if signal.Account != "" {
			fmt.Fprintf(&sb, "  Account: %s\n", signal.Account)
		}
		if signal.LastError != "" {
			fmt.Fprintf(&sb, "  Last error: %s\n", signal.LastError)
		}

		if options.V2Primary {
			sb.WriteString("\nServing store (v2):\n")
			if len(storedPlatforms) == 0 {
				sb.WriteString("  No messages stored\n")
			} else {
				for _, stat := range storedPlatforms {
					fmt.Fprintf(&sb, "  %s: %d messages\n", stat.Platform, stat.Count)
				}
			}
		}

		fmt.Fprintf(&sb, "Data dir: %s\n", a.DataDir)
		payload := map[string]any{
			"overall_connected": overallConnected,
			"google":            google,
			"whatsapp":          whatsApp,
			"signal":            signal,
			"data_dir":          a.DataDir,
		}
		if options.V2Primary {
			payload["stored_platforms"] = storedPlatforms
		}
		return structuredResult(payload, sb.String()), nil
	}
}
