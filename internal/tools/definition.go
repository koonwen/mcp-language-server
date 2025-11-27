package tools

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/koonwen/mcp-language-server/internal/lsp"
	"github.com/koonwen/mcp-language-server/internal/protocol"
)

// GoToDefinition finds the definition of the symbol at the given file position.
// This is the position-based approach that uses the LSP textDocument/definition request.
// Line and column are 1-indexed (will be converted to 0-indexed for LSP protocol).
func GoToDefinition(ctx context.Context, client *lsp.Client, filePath string, line, column int) (string, error) {
	// Get context lines from environment variable
	contextLines := 5
	if envLines := os.Getenv("LSP_CONTEXT_LINES"); envLines != "" {
		if val, err := strconv.Atoi(envLines); err == nil && val >= 0 {
			contextLines = val
		}
	}

	// Open the file if not already open
	err := client.OpenFile(ctx, filePath)
	if err != nil {
		return "", fmt.Errorf("could not open file: %v", err)
	}

	// Convert 1-indexed line/column to 0-indexed for LSP protocol
	uri := protocol.DocumentUri("file://" + filePath)
	position := protocol.Position{
		Line:      uint32(line - 1),
		Character: uint32(column - 1),
	}

	// Use LSP definition request with position-based params
	defParams := protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{
				URI: uri,
			},
			Position: position,
		},
	}

	result, err := client.Definition(ctx, defParams)
	if err != nil {
		return "", fmt.Errorf("failed to get definition: %v", err)
	}

	// Extract locations from the result
	// The result can be Definition (Or_Definition containing Location or []Location) or []DefinitionLink
	var locations []protocol.Location
	if result.Value != nil {
		switch v := result.Value.(type) {
		case protocol.Definition:
			// Definition is Or_Definition which contains Location or []Location
			if v.Value != nil {
				switch inner := v.Value.(type) {
				case protocol.Location:
					locations = append(locations, inner)
				case []protocol.Location:
					locations = inner
				}
			}
		case protocol.Location:
			locations = append(locations, v)
		case []protocol.Location:
			locations = v
		case []protocol.DefinitionLink:
			for _, link := range v {
				locations = append(locations, protocol.Location{
					URI:   link.TargetURI,
					Range: link.TargetRange,
				})
			}
		}
	}

	if len(locations) == 0 {
		return fmt.Sprintf("No definition found at %s:%d:%d", filePath, line, column), nil
	}

	var definitions []string

	for _, loc := range locations {
		defFilePath := strings.TrimPrefix(string(loc.URI), "file://")

		// Open the definition file
		err := client.OpenFile(ctx, defFilePath)
		if err != nil {
			toolsLogger.Error("Error opening file: %v", err)
			continue
		}

		// Get full definition using the existing helper
		definition, expandedLoc, err := GetFullDefinition(ctx, client, loc)
		if err != nil {
			toolsLogger.Error("Error getting full definition: %v", err)
			continue
		}

		// Read file to get context
		fileContent, err := os.ReadFile(defFilePath)
		if err != nil {
			toolsLogger.Error("Error reading file: %v", err)
			continue
		}

		lines := strings.Split(string(fileContent), "\n")

		// Determine lines to show with context
		startLine := int(expandedLoc.Range.Start.Line)
		endLine := int(expandedLoc.Range.End.Line)

		// Add context lines
		contextStart := startLine - contextLines
		if contextStart < 0 {
			contextStart = 0
		}
		contextEnd := endLine + contextLines
		if contextEnd >= len(lines) {
			contextEnd = len(lines) - 1
		}

		banner := "---\n\n"
		locationInfo := fmt.Sprintf(
			"File: %s\n"+
				"Definition at: L%d:C%d - L%d:C%d\n\n",
			defFilePath,
			expandedLoc.Range.Start.Line+1,
			expandedLoc.Range.Start.Character+1,
			expandedLoc.Range.End.Line+1,
			expandedLoc.Range.End.Character+1,
		)

		definition = addLineNumbers(definition, int(expandedLoc.Range.Start.Line)+1)
		definitions = append(definitions, banner+locationInfo+definition+"\n")
	}

	if len(definitions) == 0 {
		return fmt.Sprintf("Could not read definition at %s:%d:%d", filePath, line, column), nil
	}

	return strings.Join(definitions, ""), nil
}

func ReadDefinition(ctx context.Context, client *lsp.Client, symbolName string) (string, error) {
	symbolResult, err := client.Symbol(ctx, protocol.WorkspaceSymbolParams{
		Query: symbolName,
	})
	if err != nil {
		return "", fmt.Errorf("failed to fetch symbol: %v", err)
	}

	results, err := symbolResult.Results()
	if err != nil {
		return "", fmt.Errorf("failed to parse results: %v", err)
	}

	var definitions []string
	for _, symbol := range results {
		kind := ""
		container := ""

		// Skip symbols that we are not looking for. workspace/symbol may return
		// a large number of fuzzy matches.
		switch v := symbol.(type) {
		case *protocol.SymbolInformation:
			// SymbolInformation results have richer data.
			kind = fmt.Sprintf("Kind: %s\n", protocol.TableKindMap[v.Kind])
			if v.ContainerName != "" {
				container = fmt.Sprintf("Container Name: %s\n", v.ContainerName)
			}

			// Handle different matching strategies based on the search term
			if strings.Contains(symbolName, ".") {
				// For qualified names like "Type.Method", require exact match
				if symbol.GetName() != symbolName {
					continue
				}
			} else {
				// For unqualified names like "Method"
				if v.Kind == protocol.Method {
					// For methods, only match if the method name matches exactly Type.symbolName or Type::symbolName or symbolName
					if !strings.HasSuffix(symbol.GetName(), "::"+symbolName) && !strings.HasSuffix(symbol.GetName(), "."+symbolName) && symbol.GetName() != symbolName {
						continue
					}
				} else if symbol.GetName() != symbolName {
					// For non-methods, exact match only
					continue
				}
			}
		default:
			if symbol.GetName() != symbolName {
				continue
			}
		}

		toolsLogger.Debug("Found symbol: %s", symbol.GetName())
		loc := symbol.GetLocation()

		err := client.OpenFile(ctx, loc.URI.Path())
		if err != nil {
			toolsLogger.Error("Error opening file: %v", err)
			continue
		}

		banner := "---\n\n"
		definition, loc, err := GetFullDefinition(ctx, client, loc)
		locationInfo := fmt.Sprintf(
			"Symbol: %s\n"+
				"File: %s\n"+
				kind+
				container+
				"Range: L%d:C%d - L%d:C%d\n\n",
			symbol.GetName(),
			strings.TrimPrefix(string(loc.URI), "file://"),
			loc.Range.Start.Line+1,
			loc.Range.Start.Character+1,
			loc.Range.End.Line+1,
			loc.Range.End.Character+1,
		)

		if err != nil {
			toolsLogger.Error("Error getting definition: %v", err)
			continue
		}

		definition = addLineNumbers(definition, int(loc.Range.Start.Line)+1)

		definitions = append(definitions, banner+locationInfo+definition+"\n")
	}

	if len(definitions) == 0 {
		return fmt.Sprintf("%s not found", symbolName), nil
	}

	return strings.Join(definitions, ""), nil
}
