package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/cmd"
)

// version is set at build time via -ldflags "-X main.version=v0.2.0".
// Defaults to "dev" for local builds.
var version = "dev"

func main() {
	cmd.SetVersion(version)

	level := cmd.LogLevel()
	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).
		With().Timestamp().Logger().Level(level)

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: openmessage <pair|serve|demo|backup|migrate|read|thread|threads|send|import>")
		fmt.Fprintln(os.Stderr, "  pair [--google|--google-file path]       - Pair with your phone via QR or Google account cookies")
		fmt.Fprintln(os.Stderr, "  serve [--demo] [--web|--no-web] [--mcp-sse|--no-mcp-sse] [--mcp-stdio] - Start explicit web/MCP transports")
		fmt.Fprintln(os.Stderr, "  demo                                     - Start a seeded fake-data UI with live transports disabled")
		fmt.Fprintln(os.Stderr, "  backup [--to dir] [--json]               - Create a verified legacy migration backup and manifest")
		fmt.Fprintln(os.Stderr, "  migrate [--check] [--from dir] [--to dir] [--json] - Transform the legacy store into a validated v2 store")
		fmt.Fprintln(os.Stderr, "  repair google-idspace --since <RFC3339|unix-ms> [--apply] [--json] - Re-file v2 rows misrouted by a Google device id-space reset (dry run by default)")
		fmt.Fprintln(os.Stderr, "  read <query> [--limit N] [--phone X] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--json] - Search the local store")
		fmt.Fprintln(os.Stderr, "  thread <name|number|conversation_id> [--limit N] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--json] - Print a full conversation chronologically")
		fmt.Fprintln(os.Stderr, "  threads [--limit N] [--json]             - List recent conversations (find an id/name for thread)")
		fmt.Fprintln(os.Stderr, "  status [--json]                          - Show per-platform message counts and sync freshness")
		fmt.Fprintln(os.Stderr, "  send <conversation_id> <msg> [--not-before-ms N] [--idempotency-key K] - Send text to an existing conversation")
		fmt.Fprintln(os.Stderr, "  send-group <phone1,phone2,...> <msg>       - Send group message (MMS)")
		fmt.Fprintln(os.Stderr, "  import gchat <groups-dir> [--email you@]  - Import Google Chat Takeout")
		fmt.Fprintln(os.Stderr, "  import gchat-conversation <messages.json> - Import single GChat conversation")
		fmt.Fprintln(os.Stderr, "  import imessage [db-path]                 - Import iMessage (needs Full Disk Access)")
		fmt.Fprintln(os.Stderr, "  import whatsapp <chat.txt> [--name You]   - Import WhatsApp text export")
		fmt.Fprintln(os.Stderr, "  import signal [support-dir]               - Import Signal Desktop history")
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "pair":
		err = cmd.RunPair(logger, os.Args[2:]...)
	case "serve":
		err = cmd.RunServe(logger, os.Args[2:]...)
	case "demo":
		err = cmd.RunDemo(logger)
	case "backup":
		err = cmd.RunBackup(logger, os.Args[2:]...)
	case "migrate":
		err = cmd.RunMigrate(logger, os.Args[2:]...)
	case "repair":
		err = cmd.RunRepair(logger, os.Args[2:]...)
	case "read", "search":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: openmessage read <query> [--limit N] [--phone NUMBER] [--json]")
			os.Exit(1)
		}
		err = cmd.RunRead(logger, os.Args[2:]...)
	case "thread":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: openmessage thread <name|number|conversation_id> [--limit N] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--json]")
			os.Exit(1)
		}
		err = cmd.RunThread(logger, os.Args[2:]...)
	case "threads":
		err = cmd.RunThreads(logger, os.Args[2:]...)
	case "status":
		err = cmd.RunStatus(logger, os.Args[2:]...)
	case "send":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: openmessage send <conversation_id> <message> [--not-before-ms N] [--idempotency-key K]")
			os.Exit(1)
		}
		var notBeforeMS *int64
		var idempotencyKey string
		for index := 4; index < len(os.Args); index += 2 {
			if index+1 >= len(os.Args) {
				err = fmt.Errorf("send option %s requires a value", os.Args[index])
				break
			}
			switch os.Args[index] {
			case "--not-before-ms":
				value, parseErr := strconv.ParseInt(os.Args[index+1], 10, 64)
				if parseErr != nil {
					err = fmt.Errorf("--not-before-ms must be an integer: %w", parseErr)
					break
				}
				notBeforeMS = &value
			case "--idempotency-key":
				idempotencyKey = os.Args[index+1]
			default:
				err = fmt.Errorf("unknown send option %s", os.Args[index])
			}
			if err != nil {
				break
			}
		}
		if err != nil {
			break
		}
		err = cmd.RunSendWithOptions(logger, os.Args[2], os.Args[3], cmd.SendOptions{
			NotBeforeMS: notBeforeMS, IdempotencyKey: idempotencyKey,
		})
	case "send-group":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: openmessage send-group <phone1,phone2,...> <message>")
			os.Exit(1)
		}
		phones := strings.Split(os.Args[2], ",")
		err = cmd.RunSendGroup(logger, phones, os.Args[3])
	case "import":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: openmessage import <gchat|gchat-conversation|imessage|whatsapp|signal> [args...]")
			os.Exit(1)
		}
		err = cmd.RunImport(logger, os.Args[2], os.Args[3:])
	case "debug-media":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: openmessage debug-media <conversation_id>")
			os.Exit(1)
		}
		err = cmd.RunDebugMedia(logger, os.Args[2])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		fmt.Fprintln(os.Stderr, "Usage: openmessage <pair|serve|demo|backup|migrate|read|thread|threads|send|import>")
		os.Exit(1)
	}

	if err != nil {
		if instruction := cmd.OperatorInstruction(err); instruction != "" {
			fmt.Fprintln(os.Stderr, instruction)
		}
		if exitCode := cmd.ExitCode(err); exitCode != 1 {
			logger.Error().Err(err).Msg("Command failed")
			os.Exit(exitCode)
		}
		logger.Fatal().Err(err).Msg("Fatal error")
	}
}
