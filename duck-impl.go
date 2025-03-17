package main

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"

	"golang.org/x/tools/go/packages"
)

type Method struct {
	MethodName string
	Parameters []string        // paramName paramType
	Results    []string        // resName resType
	Imports    map[string]bool // stored imports used in the method by paramType and resType
}

type Generator struct {
	StructName    string
	InterfaceName string
	OutputFile    string
	PackageName   string
	Methods       []Method
	Imports       []string // deduplicated list of imports
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
	var currentPkg string
	// Parse the current directory to get the package name
	if fset := token.NewFileSet(); fset != nil {
		pkgs, err := parser.ParseDir(fset, dir, nil, parser.PackageClauseOnly)
		if err == nil {
			for pkgName := range pkgs {
				currentPkg = pkgName
			}
		}
	}

	imports := make([]string, 0)
	// process imports
	for _, method := range methods {
		for imp, in_use := range method.Imports {
			if in_use {
				imports = append(imports, imp)
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
		Imports:       imports,
	}

	if err := generator.Generate(); err != nil {
		log.Fatalf("Failed to generate code: %v", err)
	}
}

func SplitRight(s, sep string) []string {
	idx := strings.LastIndex(s, sep)
	if idx == -1 {
		return []string{s} // separator not found
	}
	return []string{s[:idx], s[idx+len(sep):]}
}

func parseInterface(dir, interfaceName string) ([]Method, string, error) {
	// Handle potentially qualified interface name (package.Interface)
	var pkgPath, intName string
	parts := SplitRight(interfaceName, ".")
	if len(parts) > 1 {
		pkgPath = parts[0]
		intName = parts[len(parts)-1] // Use the last part as the interface name
	} else {
		intName = interfaceName
	}

	debugLog("Looking for interface: package=%s, name=%s\n", pkgPath, intName)

	// First, try using the go/packages approach (preferred)
	methods, hostPkgName, err := parseInterfaceWithTypes(dir, pkgPath, intName, interfaceName)
	if err == nil {
		return methods, hostPkgName, nil
	}

	debugLog("go/packages approach failed: %v\n", err)
	debugLog("Falling back to AST-based approach\n")

	// Fall back to the AST-based approach
	return parseInterfaceWithAST(dir, pkgPath, intName, interfaceName)
}

// parseInterfaceWithTypes uses the go/packages and go/types packages to load and analyze interfaces
func parseInterfaceWithTypes(dir, pkgPath, intName, fullInterfaceName string) ([]Method, string, error) {
	var importPath string

	if pkgPath == "" {
		// For interfaces in the current package, we need to determine the import path
		cmd := exec.Command("go", "list", "-f", "{{.ImportPath}}")
		cmd.Dir = dir // Set working directory for the command
		output, err := cmd.Output()
		if err != nil {
			return nil, "", fmt.Errorf("failed to determine current package import path: %v", err)
		}
		importPath = strings.TrimSpace(string(output))
	} else {
		// Extract the actual import path from the package path
		// For paths like "github.com/user/repo/path/to/module.Interface",
		// we need to determine the module path (could be repo or repo/path/to/module)
		importPath = pkgPath

		// Try to find the base module path by iteratively trying shorter paths
		components := strings.Split(pkgPath, "/")
		for i := len(components); i > 0; i-- {
			partialPath := strings.Join(components[:i], "/")
			if isValidModule(partialPath) {
				importPath = partialPath
				debugLog("Found valid module: %s\n", importPath)
				break
			}
		}
	}

	debugLog("Loading package: %s\n", importPath)

	// Configure the packages.Load
	cfg := &packages.Config{
		Mode:  packages.NeedName | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax,
		Dir:   dir, // Set the working directory
		Tests: false,
	}

	pkgs, err := packages.Load(cfg, importPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to load package %s: %v", importPath, err)
	}

	if len(pkgs) == 0 {
		return nil, "", fmt.Errorf("no packages found for %s", importPath)
	}

	// Check for load errors
	var errs []string
	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		for _, err := range pkg.Errors {
			errs = append(errs, err.Error())
		}
	})

	if len(errs) > 0 {
		return nil, "", fmt.Errorf("errors loading packages: %s", strings.Join(errs, "; "))
	}

	pkg := pkgs[0]
	debugLog("Package loaded: %s\n", pkg.Name)

	// Look up the interface type
	obj := pkg.Types.Scope().Lookup(intName)
	if obj == nil {
		// If not found directly, try to search in imported packages
		for _, imported := range pkg.Imports {
			obj = imported.Types.Scope().Lookup(intName)
			if obj != nil {
				pkg = imported // Use the package where the interface was found
				break
			}
		}
	}

	if obj == nil {
		return nil, "", fmt.Errorf("interface %s not found in package %s", intName, importPath)
	}

	// Verify it's an interface type
	named, ok := obj.Type().(*types.Named)
	if !ok {
		return nil, "", fmt.Errorf("%s is not a named type", intName)
	}

	iface, ok := named.Underlying().(*types.Interface)
	if !ok {
		return nil, "", fmt.Errorf("%s is not an interface type", intName)
	}

	debugLog("Found interface %s in package %s\n", intName, pkg.Name)

	// Extract methods from the interface
	var methods []Method
	for i := 0; i < iface.NumMethods(); i++ {
		meth := iface.Method(i)
		sig := meth.Type().(*types.Signature)

		method := Method{
			MethodName: meth.Name(),
		}

		// collect imports from interface's methods
		imports := make(map[string]bool)
		// Process parameters
		for j := range sig.Params().Len() {
			param := sig.Params().At(j)
			for _, import_path := range param.Pkg().Imports() {
				path := import_path.Path()
				imports[path] = strings.Contains(param.Origin().String(), path)
			}
			paramTypeStr := types.TypeString(param.Type(), func(p *types.Package) string { return p.Name() })

			// Handle variadic parameters
			if sig.Variadic() && j == sig.Params().Len()-1 {
				slice, ok := param.Type().(*types.Slice)
				if ok {
					elemTypeStr := types.TypeString(slice.Elem(), func(p *types.Package) string { return "" })
					paramTypeStr = "..." + elemTypeStr
				}
			}

			paramName := param.Name()
			if paramName == "" {
				// If the parameter has no name, use a generic name
				paramName = fmt.Sprintf("arg%d", j)
			}

			method.Parameters = append(method.Parameters, fmt.Sprintf("%s %s", paramName, paramTypeStr))
		}

		// Process return values
		for j := range sig.Results().Len() {
			result := sig.Results().At(j)
			for _, import_path := range result.Pkg().Imports() {
				path := import_path.Path()
				imports[path] = strings.Contains(result.Origin().String(), path)
			}

			resultTypeStr := types.TypeString(result.Type(), func(p *types.Package) string { return p.Name() })

			resultName := result.Name()
			if resultName == "" {
				// If the result has no name, just use the type
				method.Results = append(method.Results, resultTypeStr)
			} else {
				method.Results = append(method.Results, fmt.Sprintf("%s %s", resultName, resultTypeStr))
			}

			method.Imports = imports
		}

		methods = append(methods, method)
	}

	return methods, pkg.Name, nil
}

// isValidModule checks if the given import path is a valid Go module
func isValidModule(importPath string) bool {
	cmd := exec.Command("go", "list", "-f", "{{.Dir}}", importPath)
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

func findModulePath(importPath string) (string, error) {
	cmd := exec.Command("go", "list", "-f", importPath)
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("go list failed: %s", exitErr.Stderr)
		}
		return "", fmt.Errorf("failed to execute go list: %v", err)
	}
	debugLog("Found module path: %s\n", string(output))
	return strings.TrimSpace(string(output)), nil
}

// parseInterfaceWithAST is the original AST-based approach as a fallback
func parseInterfaceWithAST(dir, pkgPath, intName, fullInterfaceName string) ([]Method, string, error) {
	fset := token.NewFileSet()

	// Parse the package
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.ParseComments)
	if err != nil {
		return nil, "", fmt.Errorf("could not parse directory: %v", err)
	}

	var interfaceType *ast.InterfaceType
	var hostPkgName string
	var stdPkgs map[string]*ast.Package

	if pkgPath != "" {
		// Determine the full import path for the package
		importPath := pkgPath

		debugLog("Attempting to load package: %s\n", importPath)

		// First try standard library
		goRoot := runtime.GOROOT()
		stdLibPath := filepath.Join(goRoot, "src", strings.Replace(importPath, ".", "/", -1))

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

		// If not found in standard library, try to find module root first
		if interfaceType == nil {
			// Try to find the base module path by iteratively trying shorter paths
			debugLog("let's try to find the module path by iteratively trying shorter paths\n")
			components := strings.Split(pkgPath, "/")
			var modulePath string

			for i := len(components); i > 0; i-- {
				partialPath := strings.Join(components[:i], "/")
				path, err := findModulePath(partialPath)
				debugLog("path: %s, err: %v\n", path, err)
				if err == nil && path != "" {
					modulePath = path
					// If we found a valid module but need to access a subpackage
					if i < len(components) {
						modulePath = filepath.Join(modulePath, strings.Join(components[i:], "/"))
					}
					debugLog("Found module root: %s, full path: %s\n", partialPath, modulePath)
					break
				}
			}

			if modulePath != "" {
				debugLog("Found module path: %s\n", modulePath)

				// Parse the module
				modPkgs, err := parser.ParseDir(fset, modulePath, nil, parser.ParseComments)
				if err == nil {
					debugLog("Successfully parsed module directory\n")

					for modPkgName, modPkg := range modPkgs {
						debugLog("Examining package: %s\n", modPkgName)
						hostPkgName = modPkgName

						for fileName, file := range modPkg.Files {
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

								debugLog("Found interface %s in module\n", intName)
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
				} else {
					debugLog("Error parsing module directory: %v\n", err)
				}
			} else {
				debugLog("Could not find valid module path\n")
			}

			// Final fallback to the old approach
			if interfaceType == nil {
				goPath := os.Getenv("GOPATH")
				if goPath == "" {
					// Default GOPATH
					homeDir, _ := os.UserHomeDir()
					goPath = filepath.Join(homeDir, "go")
				}

				// For third-party packages
				possiblePaths := []string{
					filepath.Join(goPath, "src", strings.Replace(importPath, ".", "/", -1)),
					filepath.Join(goPath, "pkg", "mod", strings.Replace(importPath, ".", "/", -1)+"@*"), // For Go modules
					filepath.Join(dir, "vendor", strings.Replace(importPath, ".", "/", -1)),
				}

				for _, path := range possiblePaths {
					debugLog("Searching fallback path: %s\n", path)
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
		return nil, "", fmt.Errorf("interface %s not found", intName)
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

import (
{{- range .Imports}}
	"{{.}}"
{{- end}}
)

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
