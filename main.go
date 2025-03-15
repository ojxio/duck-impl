package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

type myIO = io.ReadWriteCloser

type Method struct {
	MethodName string
	Parameters []string // paramName paramType
	Results    []string // resName resType
}

type Generator struct {
	StructName    string
	InterfaceName string
	OutputFile    string
	PackageName   string
	Methods       []Method
}

func main() {
	// Parse command line flags
	structName := flag.String("struct", "", "Name of the struct to hold the implementations of the interface")
	interfaceName := flag.String("interface", "", "Name of the interface to implement")
	outputFile := flag.String("outputFile", "", "Output file name")
	flag.Parse()

	if *structName == "" || *interfaceName == "" || *outputFile == "" {
		log.Fatal("struct, interface and outputFile flags are required")
	}

	// Get current working directory
	dir, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get current directory: %v", err)
	}

	// Parse the Go files in the current directory
	methods, pkgName, err := parseInterface(dir, *interfaceName)
	if err != nil {
		log.Fatalf("Failed to parse interface: %v", err)
	}

	// Generate code
	generator := Generator{
		StructName:    *structName,
		InterfaceName: *interfaceName,
		OutputFile:    *outputFile,
		PackageName:   pkgName,
		Methods:       methods,
	}

	if err := generator.Generate(); err != nil {
		log.Fatalf("Failed to generate code: %v", err)
	}
}

func parseInterface(dir, interfaceName string) ([]Method, string, error) {
	fset := token.NewFileSet()

	// Handle potentially qualified interface name (package.Interface)
	var pkgName, intName string
	parts := strings.Split(interfaceName, ".")
	if len(parts) > 1 {
		pkgName = parts[0]
		intName = parts[1]
	} else {
		intName = interfaceName
	}

	// Parse the package
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.ParseComments)
	if err != nil {
		return nil, "", fmt.Errorf("could not parse directory: %v", err)
	}

	// First, check if we need to find an import
	var importPath string
	if pkgName != "" {
		// Look for import statement matching pkgName
		for _, pkg := range pkgs {
			for _, file := range pkg.Files {
				for _, imp := range file.Imports {
					// Check if this import matches our package
					if imp.Name != nil && imp.Name.Name == pkgName {
						// Remove quotes from Path.Value
						importPath = strings.Trim(imp.Path.Value, "\"")
						break
					} else if imp.Path != nil {
						// Extract last part of path
						pathParts := strings.Split(strings.Trim(imp.Path.Value, "\""), "/")
						if len(pathParts) > 0 && pathParts[len(pathParts)-1] == pkgName {
							importPath = strings.Trim(imp.Path.Value, "\"")
							break
						}
					}
				}
				if importPath != "" {
					break
				}
			}
			if importPath != "" {
				break
			}
		}
	}

	// If we found an import path and need to look for an external package
	var interfaceType *ast.InterfaceType
	var hostPkgName string

	if pkgName != "" && importPath != "" {
		// Try to load the package from GOPATH or Go modules
		goPath := os.Getenv("GOPATH")
		if goPath == "" {
			// Default GOPATH
			homeDir, _ := os.UserHomeDir()
			goPath = filepath.Join(homeDir, "go")
		}

		// Try different possible locations for the package
		possiblePaths := []string{
			filepath.Join(goPath, "src", importPath),
			filepath.Join(goPath, "pkg", "mod", importPath+"@v*"), // For modules
			filepath.Join(dir, "vendor", importPath),
		}

		for _, path := range possiblePaths {
			matches, _ := filepath.Glob(path)
			for _, match := range matches {
				if stat, err := os.Stat(match); err == nil && stat.IsDir() {
					// Parse the external package
					extPkgs, err := parser.ParseDir(fset, match, nil, parser.ParseComments)
					if err != nil {
						continue
					}

					// Look for the interface in the external package
					for extPkgName, extPkg := range extPkgs {
						hostPkgName = extPkgName
						for _, file := range extPkg.Files {
							ast.Inspect(file, func(n ast.Node) bool {
								typeSpec, ok := n.(*ast.TypeSpec)
								if !ok || typeSpec.Name.Name != intName {
									return true
								}

								iface, ok := typeSpec.Type.(*ast.InterfaceType)
								if !ok {
									return true
								}

								interfaceType = iface
								return false
							})
							if interfaceType != nil {
								break
							}
						}
						if interfaceType != nil {
							break
						}
					}
					if interfaceType != nil {
						break
					}
				}
			}
			if interfaceType != nil {
				break
			}
		}
	} else {
		// Look for interface in local package
		for _, pkg := range pkgs {
			hostPkgName = pkg.Name

			for _, file := range pkg.Files {
				ast.Inspect(file, func(n ast.Node) bool {
					typeSpec, ok := n.(*ast.TypeSpec)
					if !ok || typeSpec.Name.Name != intName {
						return true
					}

					iface, ok := typeSpec.Type.(*ast.InterfaceType)
					if !ok {
						return true
					}

					interfaceType = iface
					return false
				})
				if interfaceType != nil {
					break
				}
			}
			if interfaceType != nil {
				break
			}
		}
	}

	if interfaceType == nil {
		return nil, "", fmt.Errorf("interface %s not found", interfaceName)
	}

	// Rest of the function remains the same...
	methods := make([]Method, 0)
	for _, method := range interfaceType.Methods.List {
		for _, name := range method.Names {
			funcType, ok := method.Type.(*ast.FuncType)
			if !ok {
				continue
			}

			foo := Method{
				MethodName: name.Name,
				Parameters: extractParams(funcType.Params),
				Results:    extractParams(funcType.Results),
			}

			methods = append(methods, foo)
		}
	}

	return methods, hostPkgName, nil
}

func extractParams(fieldList *ast.FieldList) []string {
	if fieldList == nil {
		return []string{}
	}

	params := make([]string, 0, fieldList.NumFields())
	for _, field := range fieldList.List {
		typeStr := formatNode(field.Type)

		// If there are names, use them
		if len(field.Names) > 0 {
			for _, name := range field.Names {
				params = append(params, fmt.Sprintf("%s %s", name.Name, typeStr))
			}
		} else {
			// For unnamed returns
			params = append(params, typeStr)
		}
	}

	return params
}

func formatNode(node ast.Expr) string {
	switch n := node.(type) {
	case *ast.Ident:
		return n.Name
	case *ast.SelectorExpr:
		return formatNode(n.X) + "." + n.Sel.Name
	case *ast.StarExpr:
		return "*" + formatNode(n.X)
	case *ast.ArrayType:
		if n.Len == nil {
			return "[]" + formatNode(n.Elt)
		}
		return "[" + formatNode(n.Len) + "]" + formatNode(n.Elt)
	case *ast.MapType:
		return "map[" + formatNode(n.Key) + "]" + formatNode(n.Value)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.FuncType:
		return "func" + formatFuncParams(n.Params) + formatFuncResults(n.Results)
	case *ast.BasicLit:
		return n.Value
	case *ast.ChanType:
		switch n.Dir {
		case ast.SEND:
			return "chan<- " + formatNode(n.Value)
		case ast.RECV:
			return "<-chan " + formatNode(n.Value)
		default:
			return "chan " + formatNode(n.Value)
		}
	default:
		return fmt.Sprintf("/* unsupported: %T */", node)
	}
}

func formatFuncParams(fields *ast.FieldList) string {
	if fields == nil {
		return "()"
	}

	params := make([]string, 0, fields.NumFields())
	for _, field := range fields.List {
		typeStr := formatNode(field.Type)

		if len(field.Names) > 0 {
			for _, name := range field.Names {
				params = append(params, fmt.Sprintf("%s %s", name.Name, typeStr))
			}
		} else {
			params = append(params, typeStr)
		}
	}

	return "(" + strings.Join(params, ", ") + ")"
}

func formatFuncResults(fields *ast.FieldList) string {
	if fields == nil || fields.NumFields() == 0 {
		return ""
	}

	if fields.NumFields() == 1 && len(fields.List[0].Names) == 0 {
		return " " + formatNode(fields.List[0].Type)
	}

	params := make([]string, 0, fields.NumFields())
	for _, field := range fields.List {
		typeStr := formatNode(field.Type)

		if len(field.Names) > 0 {
			for _, name := range field.Names {
				params = append(params, fmt.Sprintf("%s %s", name.Name, typeStr))
			}
		} else {
			params = append(params, typeStr)
		}
	}

	return " (" + strings.Join(params, ", ") + ")"
}

// Method signature formatting functions
func (g *Generator) formatMethodParams(params []string) string {
	if len(params) == 0 {
		return "()"
	}
	return "(" + strings.Join(params, ", ") + ")"
}

func (g *Generator) formatMethodResults(results []string) string {
	if len(results) == 0 {
		return ""
	}
	return " (" + strings.Join(results, ", ") + ")"
}

const tmpl = `// Code generated by duck-impl; DO NOT EDIT.

package {{.PackageName}}

type _{{.InterfaceName}}_ struct {
{{- range .Methods}}
	{{.MethodName}}_ func{{formatParams .Parameters}}{{formatResults .Results}}
{{- end}}
}

{{- range .Methods}}

func ({{$.InterfaceName | toLower}}_impl _{{$.InterfaceName}}_) {{.MethodName}}{{formatParams .Parameters}}{{formatResults .Results}} {
	{{if hasResults .Results}}return {{end}}{{$.InterfaceName | toLower}}_impl.{{.MethodName}}_{{callParams .Parameters}}
}
{{- end}}

type {{.StructName}} = _{{.InterfaceName}}_
`

func (g *Generator) Generate() error {
	// Create template
	tmpl := template.Must(
		template.New("codegen").Funcs(template.FuncMap{
			"toLower":       strings.ToLower,
			"formatParams":  g.formatMethodParams,
			"formatResults": g.formatMethodResults,
			"callParams": func(params []string) string {
				if len(params) == 0 {
					return "()"
				}

				paramNames := make([]string, len(params))
				for i, param := range params {
					parts := strings.SplitN(param, " ", 2)
					paramNames[i] = parts[0]
				}

				return "(" + strings.Join(paramNames, ", ") + ")"
			},
			"hasResults": func(results []string) bool {
				return len(results) > 0
			},
		}).Parse(tmpl))

	// Create output file
	file, err := os.Create(g.OutputFile)
	if err != nil {
		return fmt.Errorf("could not create output file: %v", err)
	}
	defer file.Close()

	// Execute template
	err = tmpl.Execute(file, g)
	if err != nil {
		return fmt.Errorf("could not execute template: %v", err)
	}

	return nil
}
