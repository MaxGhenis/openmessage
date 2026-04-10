package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/importer"
)

// RunImport handles the "openmessage import <source> [path]" command.
func RunImport(logger zerolog.Logger, source string, args []string) error {
	a, err := app.New(logger)
	if err != nil {
		return fmt.Errorf("init app: %w", err)
	}
	defer a.Close()

	switch source {
	case "gchat":
		if len(args) < 1 {
			return fmt.Errorf("usage: openmessage import gchat <path-to-groups-dir> [--email your@email.com]")
		}
		dirPath := args[0]
		myEmail := flagValue(args[1:], "--email")
		result, err := importer.ImportGChatDirectory(a.Store, dirPath, myEmail)
		if err != nil {
			return fmt.Errorf("import gchat: %w", err)
		}
		printResult("Google Chat", result)
		return nil

	case "gchat-conversation":
		if len(args) < 1 {
			return fmt.Errorf("usage: openmessage import gchat-conversation <path-to-messages.json> [--email your@email.com]")
		}
		filePath := args[0]
		myEmail := flagValue(args[1:], "--email")
		f, err := os.Open(filePath)
		if err != nil {
			return fmt.Errorf("open file: %w", err)
		}
		defer f.Close()
		imp := &importer.GChat{MyEmail: myEmail}
		result, err := imp.Import(a.Store, f)
		if err != nil {
			return fmt.Errorf("import gchat conversation: %w", err)
		}
		printResult("Google Chat conversation", result)
		return nil

	case "imessage":
		dbPath := ""
		myName := "Me"
		if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
			dbPath = args[0]
		}
		if v := flagValue(args, "--name"); v != "" {
			myName = v
		}
		imp := &importer.IMessage{DBPath: dbPath, MyName: myName}
		result, err := imp.ImportFromDB(a.Store)
		if err != nil {
			return fmt.Errorf("import imessage: %w", err)
		}
		printResult("iMessage", result)
		return nil

	case "whatsapp":
		// Check if first arg looks like a file path (text export) or if no path given (native DB)
		if len(args) >= 1 && !strings.HasPrefix(args[0], "--") {
			// Text export mode
			filePath := args[0]
			myName := flagValue(args[1:], "--name")
			f, err := os.Open(filePath)
			if err != nil {
				return fmt.Errorf("open file: %w", err)
			}
			defer f.Close()
			imp := &importer.WhatsApp{MyName: myName}
			result, err := imp.Import(a.Store, f)
			if err != nil {
				return fmt.Errorf("import whatsapp: %w", err)
			}
			printResult("WhatsApp (text export)", result)
			return nil
		}
		// Native DB mode (reads WhatsApp Desktop's ChatStorage.sqlite)
		dbPath := flagValue(args, "--db")
		myName := flagValue(args, "--name")
		if myName == "" {
			myName = "Me"
		}
		imp := &importer.WhatsAppNative{DBPath: dbPath, MyName: myName}
		// --full disables incremental sync
		if hasFlag(args, "--full") {
			imp.SinceMS = -1 // negative = explicitly full
		}
		result, err := imp.ImportFromDB(a.Store)
		if err != nil {
			return fmt.Errorf("import whatsapp native: %w", err)
		}
		printResult("WhatsApp (native)", result)
		return nil

	case "signal":
		supportDir := ""
		if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
			supportDir = args[0]
		}
		myName := flagValue(args, "--name")
		if myName == "" {
			myName = "Me"
		}
		myAddress := flagValue(args, "--account")
		imp := &importer.SignalDesktop{
			SupportDir: supportDir,
			MyName:     myName,
			MyAddress:  myAddress,
		}
		if hasFlag(args, "--full") {
			imp.SinceMS = -1
		}
		result, err := imp.ImportFromDB(a.Store)
		if err != nil {
			return fmt.Errorf("import signal desktop: %w", err)
		}
		printResult("Signal Desktop", result)
		return nil

	default:
		return fmt.Errorf("unknown import source: %s\nSupported: gchat, gchat-conversation, imessage, whatsapp, signal", source)
	}
}

func printResult(source string, result *importer.ImportResult) {
	fmt.Printf("\n%s import complete:\n", source)
	fmt.Printf("  Conversations created: %d\n", result.ConversationsCreated)
	fmt.Printf("  Messages imported:     %d\n", result.MessagesImported)
	fmt.Printf("  Messages duplicate:    %d\n", result.MessagesDuplicate)
	if len(result.Errors) > 0 {
		fmt.Printf("  Errors:                %d\n", len(result.Errors))
		for _, e := range result.Errors {
			fmt.Printf("    - %s\n", e)
		}
	}
}

// hasFlag checks if a boolean flag is present in args.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// flagValue extracts the value after a flag like --email from args.
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, flag+"=") {
			return strings.TrimPrefix(a, flag+"=")
		}
	}
	return ""
}
