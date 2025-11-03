package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	gqlparser "github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
	"gopkg.in/yaml.v3"
)

// simple program to convert a GraphQL query + schema into an OpenAPI spec and a curl example
// Usage:
//   all-the-curls --schema schema.graphql --query myquery.graphql --endpoint https://api.example.com/graphql --out openapi.yaml --format yaml
// Optional:
//   --operation NameOfOperation (if multiple operations present)
//   --vars-file variables.json (to include real example values)
//   --title "My GraphQL as REST" --version 1.0.0

func main() {
	var schemaPath string
	var queryPath string
	var endpoint string
	var operationName string
	var outPath string
	var format string
	var varsFile string
	var title string
	var version string
	var interactive bool

	flag.StringVar(&schemaPath, "schema", "", "Path to GraphQL schema SDL (.graphql/.gql)")
	flag.StringVar(&queryPath, "query", "", "Path to GraphQL query document (.graphql/.gql)")
	flag.StringVar(&endpoint, "endpoint", "", "GraphQL HTTP endpoint URL")
	flag.StringVar(&operationName, "operation", "", "Operation name to document (if multiple in query doc)")
	flag.StringVar(&outPath, "out", "", "Output path for OpenAPI spec (stdout if empty)")
	flag.StringVar(&format, "format", "yaml", "OpenAPI output format: yaml or json")
	flag.StringVar(&varsFile, "vars-file", "", "Optional JSON file with example variable values")
	flag.StringVar(&title, "title", "GraphQL as REST", "OpenAPI document title")
	flag.StringVar(&version, "version", "1.0.0", "OpenAPI document version")
	flag.BoolVar(&interactive, "interactive", false, "Prompt for missing inputs interactively")
	flag.Parse()

	if (schemaPath == "" || queryPath == "" || endpoint == "") && interactiveEnabled(interactive) {
		fmt.Println("Interactive mode: let's collect the missing inputs.")
		if schemaPath == "" {
			schemaPath = promptExistingFile("Path to GraphQL schema SDL (.graphql/.gql)")
		}
		if queryPath == "" {
			queryPath = promptExistingFile("Path to GraphQL query document (.graphql/.gql)")
		}
		if endpoint == "" {
			endpoint = promptString("GraphQL HTTP endpoint URL", "")
		}
	}
	if schemaPath == "" || queryPath == "" || endpoint == "" {
		fatalf("--schema, --query, and --endpoint are required")
	}

	schemaSDL, err := ioutil.ReadFile(schemaPath)
	if err != nil {
		fatalf("failed to read schema: %v", err)
	}
	gqlSchema, err := gqlparser.LoadSchema(&ast.Source{Name: path.Base(schemaPath), Input: string(schemaSDL)})
	if err != nil {
		fatalf("failed to parse schema: %v", err)
	}

	queryDocBytes, err := ioutil.ReadFile(queryPath)
	if err != nil {
		fatalf("failed to read query: %v", err)
	}
	queryDoc, err := parser.ParseQuery(&ast.Source{Name: path.Base(queryPath), Input: string(queryDocBytes)})
	if err != nil {
		fatalf("failed to parse query: %v", err)
	}

	// If multiple operations and none specified, optionally prompt in interactive mode
	if operationName == "" && interactiveEnabled(interactive) && len(queryDoc.Operations) > 1 {
		names := make([]string, 0, len(queryDoc.Operations))
		for _, o := range queryDoc.Operations {
			if o.Name != "" {
				names = append(names, o.Name)
			}
		}
		if len(names) > 0 {
			operationName = promptChoice("Select operation", names)
		}
	}

	op := selectOperation(queryDoc, operationName)
	if op == nil {
		fatalf("operation not found. Provide --operation if multiple operations exist")
	}

	var exampleVars map[string]any
	if varsFile == "" && interactiveEnabled(interactive) {
		if promptYesNo("Provide a variables JSON file?", false) {
			varsFile = promptExistingFile("Path to variables JSON file")
		}
	}
	if varsFile != "" {
		b, err := ioutil.ReadFile(varsFile)
		if err != nil {
			fatalf("failed to read vars-file: %v", err)
		}
		if err := json.Unmarshal(b, &exampleVars); err != nil {
			fatalf("vars-file is not valid JSON: %v", err)
		}
	}

	// Build JSON Schema for variables
	varSchemaRef, required := buildVariablesSchema(gqlSchema, op)

	// Build an example variables object if none provided
	if exampleVars == nil {
		exampleVars = buildVariablesExample(gqlSchema, op)
	}

	spec, err := buildOpenAPISpec(title, version, endpoint, string(queryDocBytes), varSchemaRef, required)
	if err != nil {
		fatalf("failed to build OpenAPI spec: %v", err)
	}

	// Attach example request
	reqExample := map[string]any{
		"query":     string(queryDocBytes),
		"variables": exampleVars,
	}
	if spec.Paths != nil {
		for _, pi := range spec.Paths.Map() {
			if pi.Post != nil && pi.Post.RequestBody != nil {
				for ct, m := range pi.Post.RequestBody.Value.Content {
					if strings.Contains(ct, "json") {
						m.Example = reqExample
					}
				}
			}
		}
	}

	// In interactive mode, ask about output if missing
	if outPath == "" && interactiveEnabled(interactive) {
		if promptYesNo("Write OpenAPI spec to a file?", true) {
			outPath = promptString("Output path (e.g., openapi.yaml)", "openapi.yaml")
			// Infer format from extension if not explicitly set by user
			ext := strings.ToLower(path.Ext(outPath))
			if ext == ".json" {
				format = "json"
			} else if ext == ".yml" || ext == ".yaml" {
				format = "yaml"
			}
		}
	}

	// Write spec
	if err := writeSpec(spec, outPath, format); err != nil {
		fatalf("failed to write spec: %v", err)
	}

	// Print curl example to stdout
	curl := buildCurl(endpoint, string(queryDocBytes), exampleVars)
	fmt.Println("\n# Example curl:\n" + curl)
}

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(2)
}

func selectOperation(doc *ast.QueryDocument, name string) *ast.OperationDefinition {
	if name != "" {
		for _, op := range doc.Operations {
			if op.Name == name {
				return op
			}
		}
		return nil
	}
	if len(doc.Operations) == 1 {
		return doc.Operations[0]
	}
	if len(doc.Operations) > 1 {
		// prefer first named operation
		for _, op := range doc.Operations {
			if op.Name != "" {
				return op
			}
		}
		return doc.Operations[0]
	}
	return nil
}

func buildVariablesSchema(schema *ast.Schema, op *ast.OperationDefinition) (*openapi3.SchemaRef, []string) {
	properties := make(map[string]*openapi3.SchemaRef)
	required := make([]string, 0)
	for _, v := range op.VariableDefinitions {
		name := v.Variable
		ref := graphqlTypeToJSONSchema(schema, v.Type)
		properties[name] = ref
		if v.Type.NonNull {
			required = append(required, name)
		}
	}
	obj := openapi3.NewObjectSchema()
	obj.Properties = properties
	if len(required) > 0 {
		obj.Required = required
	}
	return &openapi3.SchemaRef{Value: obj}, required
}

func graphqlTypeToJSONSchema(schema *ast.Schema, t *ast.Type) *openapi3.SchemaRef {
	if t.Elem != nil { // list
		items := graphqlTypeToJSONSchema(schema, t.Elem)
 	arr := openapi3.NewArraySchema()
	arr.Items = items
	if t.NonNull {
		arr.Nullable = false
	}
	return &openapi3.SchemaRef{Value: arr}
	}
	named := t.Name()
	// non-null only affects required at parent level; we can keep type as is
	switch named {
	case "Int":
		return openapi3.NewIntegerSchema().NewRef()
	case "Float":
		return openapi3.NewFloat64Schema().NewRef()
	case "String":
		return openapi3.NewStringSchema().NewRef()
	case "Boolean":
		return openapi3.NewBoolSchema().NewRef()
	case "ID":
		s := openapi3.NewStringSchema()
		s.Description = "GraphQL ID"
		return s.NewRef()
	default:
		// enum, input object, or custom scalar
		if def := schema.Types[named]; def != nil {
			if def.Kind == ast.Enum {
				vals := make([]any, 0, len(def.EnumValues))
				for _, ev := range def.EnumValues {
					vals = append(vals, ev.Name)
				}
				s := openapi3.NewStringSchema()
				s.Enum = vals
				return s.NewRef()
			}
			if def.Kind == ast.InputObject {
				props := map[string]*openapi3.SchemaRef{}
				req := []string{}
				for _, f := range def.Fields {
					props[f.Name] = graphqlTypeToJSONSchema(schema, f.Type)
					if f.Type.NonNull {
						req = append(req, f.Name)
					}
				}
				s := openapi3.NewObjectSchema()
				s.Properties = props
				if len(req) > 0 {
					s.Required = req
				}
				return &openapi3.SchemaRef{Value: s}
			}
			// custom scalar, treat as string
			s := openapi3.NewStringSchema()
			s.Description = fmt.Sprintf("GraphQL custom scalar %s", named)
			return s.NewRef()
		}
		// fallback
		s := openapi3.NewStringSchema()
		s.Description = fmt.Sprintf("GraphQL type %s", named)
		return s.NewRef()
	}
}

func buildVariablesExample(schema *ast.Schema, op *ast.OperationDefinition) map[string]any {
	out := map[string]any{}
	for _, v := range op.VariableDefinitions {
		out[v.Variable] = exampleForType(schema, v.Type)
	}
	return out
}

func exampleForType(schema *ast.Schema, t *ast.Type) any {
	if t.Elem != nil { // list
		return []any{exampleForType(schema, t.Elem)}
	}
	switch t.Name() {
	case "Int":
		return 0
	case "Float":
		return 0.0
	case "String":
		return "string"
	case "Boolean":
		return true
	case "ID":
		return "id"
	default:
		if def := schema.Types[t.Name()]; def != nil {
			if def.Kind == ast.Enum {
				if len(def.EnumValues) > 0 {
					return def.EnumValues[0].Name
				}
				return "VALUE"
			}
			if def.Kind == ast.InputObject {
				obj := map[string]any{}
				for _, f := range def.Fields {
					obj[f.Name] = exampleForType(schema, f.Type)
				}
				return obj
			}
		}
		return "string"
	}
}

func buildOpenAPISpec(title, version, endpoint, query string, varsSchema *openapi3.SchemaRef, required []string) (*openapi3.T, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	serverURL := fmt.Sprintf("%s://%s", u.Scheme, u.Host)
	p := u.Path
	if p == "" {
		p = "/graphql"
	}

	spec := &openapi3.T{
		OpenAPI: "3.0.3",
		Info: &openapi3.Info{
			Title:   title,
			Version: version,
			Description: fmt.Sprintf("Auto-generated from GraphQL query.\n\nThis endpoint wraps the GraphQL operation as a REST-like POST."),
		},
		Servers: openapi3.Servers{{URL: serverURL}},
		Paths:   openapi3.NewPaths(),
	}

	// Request body schema: { query: string, variables: <varsSchema> }
	reqSchema := openapi3.NewObjectSchema()
	reqSchema.Properties = map[string]*openapi3.SchemaRef{
		"query":     openapi3.NewStringSchema().NewRef(),
		"variables": varsSchema,
	}
	reqSchema.Required = []string{"query"}
	content := openapi3.NewContentWithJSONSchema(reqSchema)

	respData := openapi3.NewObjectSchema()
	respErr := openapi3.NewArraySchema()
	respErr.Items = &openapi3.SchemaRef{Value: openapi3.NewObjectSchema()}
	respSchema := openapi3.NewObjectSchema()
	respSchema.Properties = map[string]*openapi3.SchemaRef{
		"data":   {Value: respData},
		"errors": {Value: respErr},
	}

	op := &openapi3.Operation{
		Summary:     fmt.Sprintf("Invoke GraphQL operation %s", opNameOrDefault(query)),
		Description: "Send the GraphQL query and variables as JSON.",
		RequestBody: &openapi3.RequestBodyRef{Value: &openapi3.RequestBody{Required: true, Content: content}},
	}
	responses := openapi3.NewResponses()
	resp := &openapi3.Response{Description: ptr("OK")}
	resp.Content = openapi3.NewContentWithJSONSchema(respSchema)
	responses.Set("200", &openapi3.ResponseRef{Value: resp})
	op.Responses = responses

	pi := &openapi3.PathItem{}
	pi.Post = op
	spec.Paths.Set(p, pi)
	return spec, nil
}

func ptr[T any](v T) *T { return &v }

func opNameOrDefault(q string) string {
	// try to extract operation name for summary
	// e.g., query GetUser($id: ID!) { ... }
	q = strings.TrimSpace(q)
	first := strings.SplitN(q, "\n", 2)[0]
	first = strings.TrimSpace(first)
	parts := strings.Fields(first)
	if len(parts) >= 2 {
		// if first token is query|mutation|subscription, second might be name
		if parts[0] == "query" || parts[0] == "mutation" || parts[0] == "subscription" {
			// if second starts with '(', then unnamed
			if !strings.HasPrefix(parts[1], "(") {
				return parts[1]
			}
		}
	}
	return "(anonymous)"
}

func writeSpec(spec *openapi3.T, outPath, format string) error {
	var b []byte
	var err error
	switch strings.ToLower(format) {
	case "yaml", "yml":
		b, err = yaml.Marshal(spec)
	case "json":
		b, err = spec.MarshalJSON()
	default:
		return errors.New("unknown format: " + format)
	}
	if err != nil {
		return err
	}
	if outPath == "" {
		os.Stdout.Write(b)
		fmt.Println()
		return nil
	}
	return ioutil.WriteFile(outPath, b, 0o644)
}

func buildCurl(endpoint, query string, variables map[string]any) string {
	body := map[string]any{
		"query":     query,
		"variables": variables,
	}
	b, _ := json.Marshal(body)
	return fmt.Sprintf("curl -X POST %s -H 'Content-Type: application/json' -d '%s'", shellEscape(endpoint), shellEscape(string(b)))
}

func shellEscape(s string) string {
	// naive single-quote escaping for POSIX shells
	return strings.ReplaceAll(s, "'", "'\\''")
}

// ---- Interactive helpers ----

func interactiveEnabled(flagSet bool) bool {
	if !flagSet {
		return false
	}
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	// Only enable prompts when stdin is a char device (TTY)
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func promptString(label, def string) string {
	reader := bufio.NewReader(os.Stdin)
	for {
		if def != "" {
			fmt.Printf("%s [%s]: ", label, def)
		} else {
			fmt.Printf("%s: ", label)
		}
		text, _ := reader.ReadString('\n')
		text = strings.TrimSpace(text)
		if text == "" {
			if def != "" {
				return def
			}
			continue
		}
		return text
	}
}

func promptExistingFile(label string) string {
	for {
		p := promptString(label, "")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		fmt.Println("File not found, please try again.")
	}
}

func promptYesNo(label string, def bool) bool {
	defStr := "y"
	if !def {
		defStr = "n"
	}
	for {
		ans := strings.ToLower(promptString(label+" (y/n)", defStr))
		if ans == "y" || ans == "yes" {
			return true
		}
		if ans == "n" || ans == "no" {
			return false
		}
		fmt.Println("Please answer y or n.")
	}
}

func promptChoice(label string, options []string) string {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Println(label + ":")
		for i, opt := range options {
			fmt.Printf("  %d) %s\n", i+1, opt)
		}
		fmt.Printf("Enter number 1-%d: ", len(options))
		text, _ := reader.ReadString('\n')
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		// simple parse
		var idx int
		for i := 0; i < len(options); i++ {
			if fmt.Sprintf("%d", i+1) == text {
				idx = i
				break
			}
		}
		if idx >= 0 && idx < len(options) && fmt.Sprintf("%d", idx+1) == text {
			return options[idx]
		}
		fmt.Println("Invalid selection, try again.")
	}
}
