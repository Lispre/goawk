// Resolve function calls and variable types

package parser

import (
	"fmt"
	"sort"

	. "github.com/benhoyt/goawk/internal/ast"
	. "github.com/benhoyt/goawk/lexer"
)

type varType int

const (
	typeUnknown varType = iota
	typeScalar
	typeArray
)

// typeInfo records type information for a single variable
type typeInfo struct {
	typ      varType
	ref      *VarExpr
	scope    VarScope
	index    int
	callName string
	argIndex int
}

// Used by printVarTypes when debugTypes is turned on
func (t typeInfo) String() string {
	var typ string
	switch t.typ {
	case typeScalar:
		typ = "Scalar"
	case typeArray:
		typ = "Array"
	default:
		typ = "Unknown"
	}
	var scope string
	switch t.scope {
	case ScopeGlobal:
		scope = "Global"
	case ScopeLocal:
		scope = "Local"
	default:
		scope = "Special"
	}
	return fmt.Sprintf("typ=%s ref=%p scope=%s index=%d callName=%q argIndex=%d",
		typ, t.ref, scope, t.index, t.callName, t.argIndex)
}

// A single variable reference (normally scalar)
type varRef struct {
	funcName string
	ref      *VarExpr
}

// A single array reference
type arrayRef struct {
	funcName string
	ref      *ArrayExpr
}

// Initialize the resolver
func (p *parser) initResolve() {
	p.varTypes = make(map[string]map[string]typeInfo)
	p.varTypes[""] = make(map[string]typeInfo) // globals
	p.functions = make(map[string]int)
	p.arrayRef("ARGV") // interpreter relies on ARGV being present
}

// Signal the start of a function: records the function name and
// local variables so variable references can determine scope
func (p *parser) startFunction(name string, params []string) {
	p.funcName = name
	p.varTypes[name] = make(map[string]typeInfo)
	p.locals = make(map[string]bool, len(params))
	for _, param := range params {
		p.locals[param] = true
	}
}

// Signal the end of a function
func (p *parser) stopFunction() {
	p.funcName = ""
	p.locals = nil
}

// Add function by name with given index
func (p *parser) addFunction(name string, index int) {
	p.functions[name] = index
}

// Records a call to a user function (for resolving indexes later)
type userCall struct {
	call *UserCallExpr
	pos  Position
}

// Record a user call site
func (p *parser) recordUserCall(call *UserCallExpr, pos Position) {
	p.userCalls = append(p.userCalls, userCall{call, pos})
}

// After parsing, resolve all user calls to their indexes. Also
// ensures functions called have actually been defined, and that
// they're not being called with too many arguments.
func (p *parser) resolveUserCalls(prog *Program) {
	for _, c := range p.userCalls {
		index, ok := p.functions[c.call.Name]
		if !ok {
			panic(&ParseError{c.pos, fmt.Sprintf("undefined function %q", c.call.Name)})
		}
		function := prog.Functions[index]
		if len(c.call.Args) > len(function.Params) {
			panic(&ParseError{c.pos, fmt.Sprintf("%q called with more arguments than declared", c.call.Name)})
		}
		c.call.Index = index
	}
}

// For arguments that are variable references, we don't know the
// type based on context, so mark the types for these as unknown.
func (p *parser) processUserCallArg(funcName string, arg Expr, index int) {
	if varExpr, ok := arg.(*VarExpr); ok {
		ref := p.varTypes[p.funcName][varExpr.Name].ref
		if ref == varExpr {
			// Only applies if this is the first reference to this
			// variable (other we know the type already)
			scope := p.varTypes[p.funcName][varExpr.Name].scope
			p.varTypes[p.funcName][varExpr.Name] = typeInfo{typeUnknown, ref, scope, 0, funcName, index}
		}
	}
}

// Determine scope of given variable reference (and funcName if it's
// a local, otherwise empty string)
func (p *parser) getScope(name string) (VarScope, string) {
	switch {
	case p.funcName != "" && p.locals[name]:
		return ScopeLocal, p.funcName
	case SpecialVarIndex(name) > 0:
		return ScopeSpecial, ""
	default:
		return ScopeGlobal, ""
	}
}

// Record a variable (scalar) reference and return the *VarExpr (but
// VarExpr.Index won't be set till later)
func (p *parser) varRef(name string) *VarExpr {
	scope, funcName := p.getScope(name)
	expr := &VarExpr{scope, 0, name}
	p.varRefs = append(p.varRefs, varRef{funcName, expr})
	typ := p.varTypes[funcName][name].typ
	if typ == typeUnknown {
		p.varTypes[funcName][name] = typeInfo{typeScalar, expr, scope, 0, "", 0}
	}
	return expr
}

// Record an array reference and return the *ArrayExpr (but
// ArrayExpr.Index won't be set till later)
func (p *parser) arrayRef(name string) *ArrayExpr {
	scope, funcName := p.getScope(name)
	expr := &ArrayExpr{scope, 0, name}
	p.arrayRefs = append(p.arrayRefs, arrayRef{funcName, expr})
	typ := p.varTypes[funcName][name].typ
	if typ == typeUnknown {
		p.varTypes[funcName][name] = typeInfo{typeArray, nil, scope, 0, "", 0}
	}
	return expr
}

// Print variable type information (for debugging) on p.debugWriter
func (p *parser) printVarTypes() {
	funcNames := []string{}
	for funcName := range p.varTypes {
		funcNames = append(funcNames, funcName)
	}
	sort.Strings(funcNames)
	for _, funcName := range funcNames {
		if funcName != "" {
			fmt.Fprintf(p.debugWriter, "function %s\n", funcName)
		} else {
			fmt.Fprintf(p.debugWriter, "globals\n")
		}
		varNames := []string{}
		for name := range p.varTypes[funcName] {
			varNames = append(varNames, name)
		}
		sort.Strings(varNames)
		for _, name := range varNames {
			info := p.varTypes[funcName][name]
			fmt.Fprintf(p.debugWriter, "  %s: %s\n", name, info)
		}
	}
}

// If we can't finish resolving after this many iterations, give up
const maxResolveIterations = 10000

// Resolve unknown variables types and generate variable indexes and
// name-to-index mappings for interpreter
func (p *parser) resolveVars(prog *Program) {
	// First go through all unknown types and try to determine the
	// type from the parameter type in that function definition. May
	// need multiple passes depending on the order of functions. This
	// is not particularly efficient, but on realistic programs it's
	// not an issue.
	for i := 0; ; i++ {
		progressed := false
		for funcName, infos := range p.varTypes {
			for name, info := range infos {
				if info.typ == typeUnknown {
					paramName := prog.Functions[p.functions[info.callName]].Params[info.argIndex]
					typ := p.varTypes[info.callName][paramName].typ
					if typ != typeUnknown {
						info.typ = typ
						p.varTypes[funcName][name] = info
						progressed = true
					}
				}
				// TODO: should check here that a variable that's used
				// as a scalar isn't an array param to a user call, and
				// vice versa
			}
		}
		if !progressed {
			// If we didn't progress we're done (or trying again is
			// not going to help)
			break
		}
		if i >= maxResolveIterations {
			panic(p.error("too many iterations trying to resolve variable types"))
		}
	}

	// Resolve global variables (iteration order is undefined, so
	// assign indexes basically randomly)
	prog.Scalars = make(map[string]int)
	prog.Arrays = make(map[string]int)
	for name, info := range p.varTypes[""] {
		var index int
		if info.scope == ScopeSpecial {
			index = SpecialVarIndex(name)
		} else if info.typ == typeScalar {
			index = len(prog.Scalars)
			prog.Scalars[name] = index
		} else {
			index = len(prog.Arrays)
			prog.Arrays[name] = index
		}
		info.index = index
		p.varTypes[""][name] = info
	}

	// Resolve local variables (assign indexes in order of params).
	// Also patch up Function.Arrays (tells interpreter which args
	// are arrays).
	for funcName, infos := range p.varTypes {
		if funcName == "" {
			continue
		}
		scalarIndex := 0
		arrayIndex := 0
		functionIndex := p.functions[funcName]
		function := prog.Functions[functionIndex]
		arrays := make([]bool, len(function.Params))
		for i, name := range function.Params {
			info := infos[name]
			var index int
			if info.typ == typeArray {
				index = arrayIndex
				arrayIndex++
				arrays[i] = true
			} else {
				// typeScalar or typeUnknown: variables may still be
				// of unknown type if they've never been referenced --
				// default to scalar in that case
				index = scalarIndex
				scalarIndex++
			}
			info.index = index
			p.varTypes[funcName][name] = info
		}
		prog.Functions[functionIndex].Arrays = arrays
	}

	if p.debugTypes {
		p.printVarTypes()
	}

	// TODO: would be nice to add errors for "can't use array as scalar"
	// and "can't use scalar as array"

	// Patch up variable indexes (interpreter uses an index instead
	// the name for more efficient lookups)
	for _, varRef := range p.varRefs {
		info := p.varTypes[varRef.funcName][varRef.ref.Name]
		varRef.ref.Index = info.index
	}
	for _, arrayRef := range p.arrayRefs {
		info := p.varTypes[arrayRef.funcName][arrayRef.ref.Name]
		arrayRef.ref.Index = info.index
	}
}