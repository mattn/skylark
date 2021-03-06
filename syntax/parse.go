// Copyright 2017 The Bazel Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package syntax

// This file defines a recursive-descent parser for Skylark.
// The LL(1) grammar of Skylark and the names of many productions follow Python 2.7.
//
// TODO(adonovan): use syntax.Error more systematically throughout the
// package.  Verify that error positions are correct using the
// chunkedfile mechanism.

import "log"

// Enable this flag to print the token stream and log.Fatal on the first error.
const debug = false

// Parse parses the input data and returns the corresponding parse tree.
//
// If src != nil, ParseFile parses the source from src and the filename
// is only used when recording position information.
// The type of the argument for the src parameter must be string,
// []byte, or io.Reader.
// If src == nil, ParseFile parses the file specified by filename.
func Parse(filename string, src interface{}) (f *File, err error) {
	in, err := newScanner(filename, src)
	if err != nil {
		return nil, err
	}
	p := parser{in: in}
	defer p.in.recover(&err)

	p.nextToken() // read first lookahead token
	f = p.parseFile()
	if f != nil {
		f.Path = filename
	}
	return f, nil
}

// ParseExpr parses a Skylark expression.
// See Parse for explanation of parameters.
func ParseExpr(filename string, src interface{}) (expr Expr, err error) {
	in, err := newScanner(filename, src)
	if err != nil {
		return nil, err
	}
	p := parser{in: in}
	defer p.in.recover(&err)

	p.nextToken() // read first lookahead token
	expr = p.parseTest()

	if p.tok != EOF {
		p.in.errorf(p.in.pos, "got %#v after expression, want EOF", p.tok)
	}

	return expr, nil
}

type parser struct {
	in     *scanner
	tok    Token
	tokval tokenValue
}

// nextToken advances the scanner and returns the position of the
// previous token.
func (p *parser) nextToken() Position {
	oldpos := p.tokval.pos
	p.tok = p.in.nextToken(&p.tokval)
	// enable to see the token stream
	if debug {
		log.Printf("nextToken: %-20s%+v\n", p.tok, p.tokval.pos)
	}
	return oldpos
}

// file_input = (NEWLINE | stmt)* EOF
func (p *parser) parseFile() *File {
	var stmts []Stmt
	for p.tok != EOF {
		if p.tok == NEWLINE {
			p.nextToken()
			continue
		}
		stmts = p.parseStmt(stmts)
	}
	return &File{Stmts: stmts}
}

func (p *parser) parseStmt(stmts []Stmt) []Stmt {
	if p.tok == DEF {
		return append(stmts, p.parseDefStmt())
	} else if p.tok == IF {
		return append(stmts, p.parseIfStmt())
	} else if p.tok == FOR {
		return append(stmts, p.parseForStmt())
	} else {
		return p.parseSimpleStmt(stmts)
	}
}

func (p *parser) parseDefStmt() Stmt {
	defpos := p.nextToken() // consume DEF
	id := p.parseIdent()
	p.consume(LPAREN)
	params := p.parseParams()
	p.consume(RPAREN)
	p.consume(COLON)
	body := p.parseSuite()
	return &DefStmt{
		Def:  defpos,
		Name: id,
		Function: Function{
			StartPos: defpos,
			Params:   params,
			Body:     body,
		},
	}
}

func (p *parser) parseIfStmt() Stmt {
	ifpos := p.nextToken() // consume IF
	cond := p.parseTest()
	p.consume(COLON)
	body := p.parseSuite()
	ifStmt := &IfStmt{
		If:   ifpos,
		Cond: cond,
		True: body,
	}
	tail := ifStmt
	for p.tok == ELIF {
		elifpos := p.nextToken() // consume ELIF
		cond := p.parseTest()
		p.consume(COLON)
		body := p.parseSuite()
		elif := &IfStmt{
			If:   elifpos,
			Cond: cond,
			True: body,
		}
		tail.ElsePos = elifpos
		tail.False = []Stmt{elif}
		tail = elif
	}
	if p.tok == ELSE {
		tail.ElsePos = p.nextToken() // consume ELSE
		p.consume(COLON)
		tail.False = p.parseSuite()
	}
	return ifStmt
}

func (p *parser) parseForStmt() Stmt {
	forpos := p.nextToken() // consume FOR
	vars := p.parseForLoopVariables()
	p.consume(IN)
	x := p.parseExpr(false)
	p.consume(COLON)
	body := p.parseSuite()
	return &ForStmt{
		For:  forpos,
		Vars: vars,
		X:    x,
		Body: body,
	}
}

// Equivalent to 'exprlist' production in Python grammar.
//
// loop_variables = primary_with_suffix (COMMA primary_with_suffix)* COMMA?
func (p *parser) parseForLoopVariables() Expr {
	// Avoid parseExpr because it would consume the IN token
	// following x in "for x in y: ...".
	v := p.parsePrimaryWithSuffix()
	if p.tok != COMMA {
		return v
	}

	list := []Expr{v}
	for p.tok == COMMA {
		p.nextToken()
		if terminatesExprList(p.tok) {
			break
		}
		list = append(list, p.parsePrimaryWithSuffix())
	}
	return &TupleExpr{List: list}
}

// simple_stmt = small_stmt (SEMI small_stmt)* SEMI? NEWLINE
func (p *parser) parseSimpleStmt(stmts []Stmt) []Stmt {
	for {
		stmts = append(stmts, p.parseSmallStmt())
		if p.tok != SEMI {
			break
		}
		p.nextToken() // consume SEMI
		if p.tok == NEWLINE || p.tok == EOF {
			break
		}
	}
	// EOF without NEWLINE occurs in `if x: pass`, for example.
	if p.tok != EOF {
		p.consume(NEWLINE)
	}
	return stmts
}

// small_stmt = RETURN expr?
//            | PASS | BREAK | CONTINUE
//            | expr ('=' | '+=' | '-=' | '*=' | '/=' | '%=') expr   // assign
//            | expr
func (p *parser) parseSmallStmt() Stmt {
	if p.tok == RETURN {
		pos := p.nextToken() // consume RETURN
		var result Expr
		if p.tok != EOF && p.tok != NEWLINE && p.tok != SEMI {
			result = p.parseExpr(false)
		}
		return &ReturnStmt{Return: pos, Result: result}
	}

	switch p.tok {
	case BREAK, CONTINUE, PASS:
		tok := p.tok
		pos := p.nextToken() // consume it
		return &BranchStmt{Token: tok, TokenPos: pos}
	}

	// Assignment
	x := p.parseExpr(false)
	switch p.tok {
	case EQ, PLUS_EQ, MINUS_EQ, STAR_EQ, SLASH_EQ, SLASHSLASH_EQ, PERCENT_EQ:
		op := p.tok
		pos := p.nextToken() // consume op
		rhs := p.parseExpr(false)
		return &AssignStmt{OpPos: pos, Op: op, LHS: x, RHS: rhs}
	}

	// Convert load(...) call into LoadStmt special form.
	// TODO(adonovan): This affects all calls to load, not just at toplevel.
	// Spec clarification needed.
	if call, ok := x.(*CallExpr); ok {
		if id, ok := call.Fn.(*Ident); ok && id.Name == "load" {
			return p.convertCallToLoad(call, id.NamePos)
		}
	}

	// Expression statement (e.g. function call, doc string).
	return &ExprStmt{X: x}
}

// Because load is not a reserved word, it is impossible to parse a load
// statement using an LL(1) grammar.  Instead we parse load(...) as a function
// call and then convert it to a LoadStmt.
func (p *parser) convertCallToLoad(call *CallExpr, loadPos Position) *LoadStmt {
	if len(call.Args) < 2 {
		p.in.errorf(call.Lparen, "load statement needs at least 2 operands, got %d", len(call.Args))
	}
	module, ok := call.Args[0].(*Literal)
	if !ok || module.Token != STRING {
		start, _ := call.Args[0].Span()
		p.in.errorf(start, "first operand of load statement must be a string literal")
	}

	// synthesize identifiers
	args := call.Args[1:]
	to := make([]*Ident, len(args))
	from := make([]*Ident, len(args))
	for i, arg := range args {
		if lit, ok := arg.(*Literal); ok && lit.Token == STRING {
			// load("module", "id")
			// To name is same as original.
			id := &Ident{
				NamePos: lit.TokenPos.add(`"`),
				Name:    lit.Value.(string),
			}
			to[i] = id
			from[i] = id
			continue
		} else if binary, ok := arg.(*BinaryExpr); ok && binary.Op == EQ {
			// load("module", to="from")
			// Symbol is locally renamed.
			if lit, ok := binary.Y.(*Literal); ok && lit.Token == STRING {
				id := &Ident{
					NamePos: lit.TokenPos.add(`"`),
					Name:    lit.Value.(string),
				}
				to[i] = binary.X.(*Ident)
				from[i] = id
				continue
			}
		}

		start, _ := arg.Span()
		p.in.errorf(start, `load operand must be "name" or localname="name"`)
	}
	return &LoadStmt{
		Load:   loadPos,
		Module: module,
		To:     to,
		From:   from,
		Rparen: call.Rparen,
	}

}

// suite is typically what follows a COLON (e.g. after DEF or FOR).
// suite = simple_stmt | NEWLINE INDENT stmt+ OUTDENT
func (p *parser) parseSuite() []Stmt {
	if p.tok == NEWLINE {
		p.nextToken() // consume NEWLINE
		p.consume(INDENT)
		var stmts []Stmt
		for p.tok != OUTDENT && p.tok != EOF {
			stmts = p.parseStmt(stmts)
		}
		p.consume(OUTDENT)
		return stmts
	}

	return p.parseSimpleStmt(nil)
}

func (p *parser) parseIdent() *Ident {
	if p.tok != IDENT {
		p.in.error(p.in.pos, "not an identifier")
	}
	id := &Ident{
		NamePos: p.tokval.pos,
		Name:    p.tokval.raw,
	}
	p.nextToken()
	return id
}

func (p *parser) consume(t Token) Position {
	if p.tok != t {
		p.in.errorf(p.in.pos, "got %#v, want %#v", p.tok, t)
	}
	return p.nextToken()
}

// params = (param COMMA)* param
//        |
//
// param = IDENT
//       | IDENT EQ test
//       | STAR IDENT
//       | STARSTAR IDENT
//
// parseParams parses a parameter list.  The resulting expressions are of the form:
//
//      *Ident
//      *Binary{Op: EQ, X: *Ident, Y: Expr}
//      *Unary{Op: STAR, X: *Ident}
//      *Unary{Op: STARSTAR, X: *Ident}
func (p *parser) parseParams() []Expr {
	var params []Expr
	stars := false
	for p.tok != RPAREN && p.tok != COLON && p.tok != EOF {
		if len(params) > 0 {
			p.consume(COMMA)
		}
		if p.tok == RPAREN {
			// list can end with a COMMA if there is neither * nor **
			if stars {
				p.in.errorf(p.in.pos, "got %#v, want parameter", p.tok)
			}
			break
		}

		// *args
		if p.tok == STAR {
			stars = true
			pos := p.nextToken()
			id := p.parseIdent()
			params = append(params, &UnaryExpr{
				OpPos: pos,
				Op:    STAR,
				X:     id,
			})
			continue
		}

		// **kwargs
		if p.tok == STARSTAR {
			stars = true
			pos := p.nextToken()
			id := p.parseIdent()
			params = append(params, &UnaryExpr{
				OpPos: pos,
				Op:    STARSTAR,
				X:     id,
			})
			continue
		}

		// IDENT
		// IDENT = test
		id := p.parseIdent()
		if p.tok == EQ { // default value
			eq := p.nextToken()
			dflt := p.parseTest()
			params = append(params, &BinaryExpr{
				X:     id,
				OpPos: eq,
				Op:    EQ,
				Y:     dflt,
			})
			continue
		}

		params = append(params, id)
	}
	return params
}

// parseExpr parses an expression, possible consisting of a
// comma-separated list of 'test' expressions.
//
// In many cases we must use parseTest to avoid ambiguity such as
// f(x, y) vs. f((x, y)).
func (p *parser) parseExpr(inParens bool) Expr {
	x := p.parseTest()
	if p.tok != COMMA {
		return x
	}

	// tuple
	exprs := p.parseExprs([]Expr{x}, inParens)
	return &TupleExpr{List: exprs}
}

// parseExprs parses a comma-separated list of expressions, starting with the comma.
// It is used to parse tuples and list elements.
// expr_list = (',' expr)* ','?
func (p *parser) parseExprs(exprs []Expr, allowTrailingComma bool) []Expr {
	for p.tok == COMMA {
		pos := p.nextToken()
		if terminatesExprList(p.tok) {
			if !allowTrailingComma {
				p.in.error(pos, "unparenthesized tuple with trailing comma")
			}
			break
		}
		exprs = append(exprs, p.parseTest())
	}
	return exprs
}

// parseTest parses a 'test', a single-component expression.
func (p *parser) parseTest() Expr {
	if p.tok == LAMBDA {
		lambda := p.nextToken()
		var params []Expr
		if p.tok != COLON {
			params = p.parseParams()
		}
		p.consume(COLON)
		body := p.parseTest()
		return &LambdaExpr{
			Lambda: lambda,
			Function: Function{
				StartPos: lambda,
				Params:   params,
				Body:     []Stmt{&ReturnStmt{Result: body}},
			},
		}
	}

	x := p.parseTestPrec(0)

	// conditional expression (t IF cond ELSE f)
	if p.tok == IF {
		ifpos := p.nextToken()
		cond := p.parseTestPrec(0)
		if p.tok != ELSE {
			p.in.error(ifpos, "conditional expression without else clause")
		}
		elsepos := p.nextToken()
		else_ := p.parseTest()
		return &CondExpr{If: ifpos, Cond: cond, True: x, ElsePos: elsepos, False: else_}
	}

	return x
}

func (p *parser) parseTestPrec(prec int) Expr {
	if prec >= len(preclevels) {
		return p.parsePrimaryWithSuffix()
	}

	// expr = NOT expr
	if p.tok == NOT && prec == int(precedence[NOT]) {
		pos := p.nextToken()
		x := p.parseTestPrec(prec + 1)
		return &UnaryExpr{
			OpPos: pos,
			Op:    NOT,
			X:     x,
		}
	}

	return p.parseBinopExpr(prec)
}

// expr = test (OP test)*
// Uses precedence climbing; see http://www.engr.mun.ca/~theo/Misc/exp_parsing.htm#climbing.
func (p *parser) parseBinopExpr(prec int) Expr {
	x := p.parseTestPrec(prec + 1)
	for first := true; ; first = false {
		if p.tok == NOT {
			p.nextToken() // consume NOT
			// In this context, NOT must be followed by IN.
			// Replace NOT IN by a single NOT_IN token.
			if p.tok != IN {
				p.in.errorf(p.in.pos, "got %#v, want in", p.tok)
			}
			p.tok = NOT_IN
		}

		// Binary operator of specified precedence?
		opprec := int(precedence[p.tok])
		if opprec < prec {
			return x
		}

		// Comparisons are non-associative.
		if !first && opprec == int(precedence[EQL]) {
			p.in.errorf(p.in.pos, "%s does not associate with %s (use parens)",
				x.(*BinaryExpr).Op, p.tok)
		}

		op := p.tok
		pos := p.nextToken()
		y := p.parseTestPrec(opprec + 1)
		x = makeBinaryExpr(op, pos, x, y)
	}
}

// precedence maps each operator to its precedence (0-7), or -1 for other tokens.
var precedence [maxToken]int8

// preclevels groups operators of equal precedence.
// Comparisons are nonassociative; other binary operators associate to the left.
// Unary MINUS and PLUS have higher precedence so are handled in parsePrimary.
// See http://docs.python.org/2/reference/expressions.html#operator-precedence
var preclevels = [...][]Token{
	{OR},  // or
	{AND}, // and
	{NOT}, // not (unary)
	{EQL, NEQ, LT, GT, LE, GE, IN, NOT_IN}, // == != < > <= >= in not in
	{PIPE},                             // |
	{AMP},                              // &
	{MINUS, PLUS},                      // -
	{STAR, PERCENT, SLASH, SLASHSLASH}, // * % / //
}

func init() {
	// populate precedence table
	for i := range precedence {
		precedence[i] = -1
	}
	for level, tokens := range preclevels {
		for _, tok := range tokens {
			precedence[tok] = int8(level)
		}
	}
}

func makeBinaryExpr(op Token, pos Position, x, y Expr) Expr {
	// Concatenate literal strings during parsing.
	if op == PLUS {
		if x, ok := x.(*Literal); ok && x.Token == STRING {
			if y, ok := y.(*Literal); ok && y.Token == STRING {
				// The Span of this synthetic node will be wrong.
				return &Literal{
					Token:    STRING,
					TokenPos: x.TokenPos,
					Raw:      x.Raw + " + " + y.Raw, // ugh
					Value:    x.Value.(string) + y.Value.(string),
				}
			}
		}
	}
	return &BinaryExpr{OpPos: pos, Op: op, X: x, Y: y}
}

// primary_with_suffix = primary
//                     | primary '.' IDENT
//                     | primary slice_suffix
//                     | primary call_suffix
func (p *parser) parsePrimaryWithSuffix() Expr {
	x := p.parsePrimary()
	for {
		switch p.tok {
		case DOT:
			dot := p.nextToken()
			id := p.parseIdent()
			x = &DotExpr{Dot: dot, X: x, Name: id}
		case LBRACK:
			x = p.parseSliceSuffix(x)
		case LPAREN:
			x = p.parseCallSuffix(x)
		default:
			return x
		}
	}
}

// slice_suffix = '[' expr? ':' expr?  ':' expr? ']'
func (p *parser) parseSliceSuffix(x Expr) Expr {
	lbrack := p.nextToken()
	var lo, hi, step Expr
	if p.tok != COLON {
		y := p.parseExpr(false)

		// index x[y]
		if p.tok == RBRACK {
			rbrack := p.nextToken()
			return &IndexExpr{X: x, Lbrack: lbrack, Y: y, Rbrack: rbrack}
		}

		lo = y
	}

	// slice or substring x[lo:hi:step]
	if p.tok == COLON {
		p.nextToken()
		if p.tok != COLON && p.tok != RBRACK {
			hi = p.parseTest()
		}
	}
	if p.tok == COLON {
		p.nextToken()
		if p.tok != RBRACK {
			step = p.parseTest()
		}
	}
	rbrack := p.consume(RBRACK)
	return &SliceExpr{X: x, Lbrack: lbrack, Lo: lo, Hi: hi, Step: step, Rbrack: rbrack}
}

// call_suffix = '(' arg_list? ')'
func (p *parser) parseCallSuffix(fn Expr) Expr {
	lparen := p.consume(LPAREN)
	var rparen Position
	var args []Expr
	if p.tok == RPAREN {
		rparen = p.nextToken()
	} else {
		args = p.parseArgs()
		rparen = p.consume(RPAREN)
	}
	return &CallExpr{Fn: fn, Lparen: lparen, Args: args, Rparen: rparen}
}

// parseArgs parses a list of actual parameter values (arguments).
// It mirrors the structure of parseParams.
// arg_list = ((arg COMMA)* arg COMMA?)?
func (p *parser) parseArgs() []Expr {
	var args []Expr
	stars := false
	for p.tok != RPAREN && p.tok != EOF {
		if len(args) > 0 {
			p.consume(COMMA)
		}
		if p.tok == RPAREN {
			// list can end with a COMMA if there is neither * nor **
			if stars {
				p.in.errorf(p.in.pos, `got %#v, want argument`, p.tok)
			}
			break
		}

		// *args
		if p.tok == STAR {
			stars = true
			pos := p.nextToken()
			x := p.parseTest()
			args = append(args, &UnaryExpr{
				OpPos: pos,
				Op:    STAR,
				X:     x,
			})
			continue
		}

		// **kwargs
		if p.tok == STARSTAR {
			stars = true
			pos := p.nextToken()
			x := p.parseTest()
			args = append(args, &UnaryExpr{
				OpPos: pos,
				Op:    STARSTAR,
				X:     x,
			})
			continue
		}

		// We use a different strategy from Bazel here to stay within LL(1).
		// Instead of looking ahead two tokens (IDENT, EQ) we parse
		// 'test = test' then check that the first was an IDENT.
		x := p.parseTest()

		if p.tok == EQ {
			// name = value
			if _, ok := x.(*Ident); !ok {
				p.in.errorf(p.in.pos, "keyword argument must have form name=expr")
			}
			eq := p.nextToken()
			y := p.parseTest()
			x = &BinaryExpr{
				X:     x,
				OpPos: eq,
				Op:    EQ,
				Y:     y,
			}
		}

		args = append(args, x)
	}
	return args
}

//  primary = IDENT
//          | INT | FLOAT
//          | STRING
//          | '[' ...                    // list literal or comprehension
//          | '{' ...                    // dict literal or comprehension
//          | '(' ...                    // tuple or parenthesized expression
//          | ('-'|'+') primary_with_suffix
func (p *parser) parsePrimary() Expr {
	switch p.tok {
	case IDENT:
		return p.parseIdent()

	case INT, FLOAT, STRING:
		var val interface{}
		tok := p.tok
		switch tok {
		case INT:
			val = p.tokval.int
		case FLOAT:
			val = p.tokval.float
		case STRING:
			val = p.tokval.string
		}
		raw := p.tokval.raw
		pos := p.nextToken()
		return &Literal{Token: tok, TokenPos: pos, Raw: raw, Value: val}

	case LBRACK:
		return p.parseList()

	case LBRACE:
		return p.parseDict()

	case LPAREN:
		lparen := p.nextToken()
		if p.tok == RPAREN {
			// empty tuple
			rparen := p.nextToken()
			return &TupleExpr{Lparen: lparen, Rparen: rparen}
		}
		e := p.parseExpr(true) // allow trailing comma
		p.consume(RPAREN)
		return e

	case MINUS, PLUS:
		// unary minus/plus:
		tok := p.tok
		pos := p.nextToken()
		x := p.parsePrimaryWithSuffix()
		return &UnaryExpr{
			OpPos: pos,
			Op:    tok,
			X:     x,
		}
	}
	p.in.errorf(p.in.pos, "got %#v, want primary expression", p.tok)
	panic("unreachable")
}

// list = '[' ']'
//      | '[' expr ']'
//      | '[' expr expr_list ']'
//      | '[' expr (FOR loop_variables IN expr)+ ']'
func (p *parser) parseList() Expr {
	lbrack := p.nextToken()
	if p.tok == RBRACK {
		// empty List
		rbrack := p.nextToken()
		return &ListExpr{Lbrack: lbrack, Rbrack: rbrack}
	}

	x := p.parseTest()

	if p.tok == FOR {
		// list comprehension
		return p.parseComprehensionSuffix(lbrack, x, RBRACK)
	}

	exprs := []Expr{x}
	if p.tok == COMMA {
		// multi-item list literal
		exprs = p.parseExprs(exprs, true) // allow trailing comma
	}

	rbrack := p.consume(RBRACK)
	return &ListExpr{Lbrack: lbrack, List: exprs, Rbrack: rbrack}
}

// dict = '{' '}'
//      | '{' dict_entry_list '}'
//      | '{' dict_entry FOR loop_variables IN expr '}'
func (p *parser) parseDict() Expr {
	lbrace := p.nextToken()
	if p.tok == RBRACE {
		// empty dict
		rbrace := p.nextToken()
		return &DictExpr{Lbrace: lbrace, Rbrace: rbrace}
	}

	x := p.parseDictEntry()

	if p.tok == FOR {
		// dict comprehension
		return p.parseComprehensionSuffix(lbrace, x, RBRACE)
	}

	entries := []Expr{x}
	for p.tok == COMMA {
		p.nextToken()
		if p.tok == RBRACE {
			break
		}
		entries = append(entries, p.parseDictEntry())
	}

	rbrace := p.consume(RBRACE)
	return &DictExpr{Lbrace: lbrace, List: entries, Rbrace: rbrace}
}

// dict_entry = test ':' test
func (p *parser) parseDictEntry() *DictEntry {
	k := p.parseTest()
	colon := p.consume(COLON)
	v := p.parseTest()
	return &DictEntry{Key: k, Colon: colon, Value: v}
}

// comp_suffix = FOR loopvars IN expr comp_suffix
//             | IF expr comp_suffix
//             | ']'  or  ')'                              (end)
//
// There can be multiple FOR/IF clauses; the first is always a FOR.
func (p *parser) parseComprehensionSuffix(lbrace Position, body Expr, endBrace Token) Expr {
	var clauses []Node
	for p.tok != endBrace {
		if p.tok == FOR {
			pos := p.nextToken()
			vars := p.parseForLoopVariables()
			in := p.consume(IN)
			// Following Python 3, the operand of IN cannot be:
			// - a conditional expression ('x if y else z'),
			//   due to conflicts in Python grammar
			//  ('if' is used by the comprehension);
			// - a lambda expression
			// - an unparenthesized tuple.
			x := p.parseTestPrec(0)
			clauses = append(clauses, &ForClause{For: pos, Vars: vars, In: in, X: x})
		} else if p.tok == IF {
			pos := p.nextToken()
			cond := p.parseTest()
			clauses = append(clauses, &IfClause{If: pos, Cond: cond})
		} else {
			p.in.errorf(p.in.pos, "got %#v, want '%s', for, or if", p.tok, endBrace)
		}
	}
	rbrace := p.nextToken()

	return &Comprehension{
		Curly:   endBrace == RBRACE,
		Lbrack:  lbrace,
		Body:    body,
		Clauses: clauses,
		Rbrack:  rbrace,
	}
}

func terminatesExprList(tok Token) bool {
	switch tok {
	case EOF, NEWLINE, EQ, RBRACE, RBRACK, RPAREN, SEMI:
		return true
	}
	return false
}
