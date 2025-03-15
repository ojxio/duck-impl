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
	"runtime"
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

var debugLog func(string, ...interface{})

func main() {
	// Parse command line flags
	structName := flag.String("struct", "", "Name of the struct to hold the implementations of the interface")
	interfaceName := flag.String("interface", "", "Name of the interface to implement")
	outputFile := flag.String("outputFile", "", "Output file name")
	debug := flag.Bool("debug", false, "Enable debug logging")
	flag.Parse()

	if *structName == "" || *interfaceName == "" || *outputFile == "" {
		log.Fatal("struct, interface and outputFile flags are required")
	}

	debugLog = func(format string, args ...interface{}) {
		if *debug {
			fmt.Printf(format, args...)
		}
	}

	// Get current working directory
	dir, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get current directory: %v", err)
	}

	// Parse the Go files in the current directory
	methods, _, err := parseInterface(dir, *interfaceName)
	if err != nil {
		log.Fatalf("Failed to parse interface: %v", err)
	}

	// get current pkg
	currentPkg := ""
	// Parse the current directory to get the package name
	if fset := token.NewFileSet(); fset != nil {
		if pkgs, err := parser.ParseDir(fset, dir, nil, 0); err == nil {
			for pkgName := range pkgs {
				currentPkg = pkgName
				break
			}
		}
	}

	// Generate code
	generator := Generator{
		StructName:    *structName,
		InterfaceName: *interfaceName,
		OutputFile:    *outputFile,
		PackageName:   currentPkg,
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
		intName = parts[len(parts)-1] // Use the last part as the interface name
	} else {
		intName = interfaceName
	}

	debugLog("Looking for interface: package=%s, name=%s\n", pkgName, intName)

	// Parse the package
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.ParseComments)
	if err != nil {
		return nil, "", fmt.Errorf("could not parse directory: %v", err)
	}

	// If we're looking for a standard library package (like io.Reader)
	var interfaceType *ast.InterfaceType
	var hostPkgName string
	var stdPkgs map[string]*ast.Package

	if pkgName != "" {
		// Try to find the package in standard library first
		targetPkg := pkgName
		if len(parts) > 2 {
			// Handle cases like "net/http.File"
			targetPkg = strings.Join(parts[:len(parts)-1], "/")
		}

		debugLog("Attempting to load package: %s\n", targetPkg)

		// Try Go's standard library path
		goRoot := runtime.GOROOT()
		stdLibPath := filepath.Join(goRoot, "src", targetPkg)

		debugLog("Searching in standard library path: %s\n", stdLibPath)

		if _, err := os.Stat(stdLibPath); err == nil {
			// Parse the standard library package
			stdPkgs, err = parser.ParseDir(fset, stdLibPath, nil, parser.ParseComments)
			if err == nil {
				for stdPkgName, stdPkg := range stdPkgs {
					debugLog("Found standard package: %s\n", stdPkgName)
					hostPkgName = stdPkgName

					// Look for the interface in the standard package
					for _, file := range stdPkg.Files {
						ast.Inspect(file, func(n ast.Node) bool {
							typeSpec, ok := n.(*ast.TypeSpec)
							if !ok || typeSpec.Name.Name != intName {
								return true
							}

							iface, ok := typeSpec.Type.(*ast.InterfaceType)
							if !ok {
								return true
							}

							debugLog("Found interface %s in standard library\n", intName)
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
		}

		// If not found in standard library, try GOPATH or Go modules
		if interfaceType == nil {
			goPath := os.Getenv("GOPATH")
			if goPath == "" {
				// Default GOPATH
				homeDir, _ := os.UserHomeDir()
				goPath = filepath.Join(homeDir, "go")
			}

			// For third-party packages
			possiblePaths := []string{
				filepath.Join(goPath, "src", targetPkg),
				filepath.Join(goPath, "pkg", "mod", targetPkg+"@*"), // For Go modules
				filepath.Join(dir, "vendor", targetPkg),
			}

			for _, path := range possiblePaths {
				debugLog("Searching path: %s\n", path)
				matches, _ := filepath.Glob(path)

				for _, match := range matches {
					if stat, err := os.Stat(match); err == nil && stat.IsDir() {
						debugLog("Found directory: %s\n", match)
						// Parse the external package
						extPkgs, err := parser.ParseDir(fset, match, nil, parser.ParseComments)
						if err != nil {
							debugLog("Error parsing directory: %v\n", err)
							continue
						}

						// Look for the interface in the external package
						for extPkgName, extPkg := range extPkgs {
							debugLog("Examining package: %s\n", extPkgName)
							hostPkgName = extPkgName

							for fileName, file := range extPkg.Files {
								debugLog("Examining file: %s\n", fileName)
								ast.Inspect(file, func(n ast.Node) bool {
									typeSpec, ok := n.(*ast.TypeSpec)
									if !ok || typeSpec.Name.Name != intName {
										return true
									}

									iface, ok := typeSpec.Type.(*ast.InterfaceType)
									if !ok {
										return true
									}

									debugLog("Found interface %s in external package\n", intName)
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
		}
	} else {
		// Look for interface in local package
		for _, pkg := range pkgs {
			hostPkgName = pkg.Name

			for fileName, file := range pkg.Files {
				debugLog("Examining local file: %s\n", fileName)
				ast.Inspect(file, func(n ast.Node) bool {
					typeSpec, ok := n.(*ast.TypeSpec)
					if !ok || typeSpec.Name.Name != intName {
						return true
					}

					iface, ok := typeSpec.Type.(*ast.InterfaceType)
					if !ok {
						return true
					}

					debugLog("Found interface %s in local package\n", intName)
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

	methods := extractMethodsFromInterface(interfaceType, fset, stdPkgs)

	return methods, hostPkgName, nil
}

// Modify the method extraction part:
func extractMethodsFromInterface(iface *ast.InterfaceType, fset *token.FileSet, stdLibPkgs map[string]*ast.Package) []Method {
	methods := make([]Method, 0)

	for _, field := range iface.Methods.List {
		// If it's a named method
		if len(field.Names) > 0 {
			for _, name := range field.Names {
				funcType, ok := field.Type.(*ast.FuncType)
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
		} else {
			// It might be an embedded interface
			switch fieldType := field.Type.(type) {
			case *ast.Ident:
				// Local embedded interface
				embeddedMethods := findEmbeddedInterfaceMethods(fieldType.Name, nil, "", fset, stdLibPkgs)
				methods = append(methods, embeddedMethods...)

			case *ast.SelectorExpr:
				// Embedded interface from another package
				if pkgIdent, ok := fieldType.X.(*ast.Ident); ok {
					embeddedMethods := findEmbeddedInterfaceMethods(fieldType.Sel.Name, pkgIdent, pkgIdent.Name, fset, stdLibPkgs)
					methods = append(methods, embeddedMethods...)
				}
			}
		}
	}

	return methods
}

func findEmbeddedInterfaceMethods(interfaceName string, pkgIdent *ast.Ident, pkgName string, fset *token.FileSet, stdLibPkgs map[string]*ast.Package) []Method {
	if pkgName != "" && stdLibPkgs[pkgName] != nil {
		// Look for the embedded interface in the standard library
		pkg := stdLibPkgs[pkgName]
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				genDecl, ok := decl.(*ast.GenDecl)
				if !ok || genDecl.Tok != token.TYPE {
					continue
				}

				for _, spec := range genDecl.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok || typeSpec.Name.Name != interfaceName {
						continue
					}

					ifaceType, ok := typeSpec.Type.(*ast.InterfaceType)
					if !ok {
						continue
					}

					return extractMethodsFromInterface(ifaceType, fset, stdLibPkgs)
				}
			}
		}
	}

	return []Method{}
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

type _{{clean .InterfaceName}}_ struct {
{{- range .Methods}}
	{{.MethodName}}_ func{{formatParams .Parameters}}{{formatResults .Results}}
{{- end}}
}

{{- range .Methods}}

func ({{clean $.InterfaceName | toLower}}_impl _{{clean $.InterfaceName}}_) {{.MethodName}}{{formatParams .Parameters}}{{formatResults .Results}} {
	{{if hasResults .Results}}return {{end}}{{clean $.InterfaceName | toLower}}_impl.{{.MethodName}}_{{callParams .Parameters}}
}
{{- end}}

type {{.StructName}} = _{{clean .InterfaceName}}_
`

func (g *Generator) Generate() error {
	// Create template
	tmpl := template.Must(
		template.New("codegen").Funcs(template.FuncMap{
			"clean": func(s string) string {
				parts := strings.Split(s, ".")
				if len(parts) > 1 {
					return parts[len(parts)-1]
				}
				return s
			},
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
