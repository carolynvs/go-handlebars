// Package lexer provides a handlebars tokenizer.
package lexer

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// References:
//   - https://github.com/wycats/handlebars.js/blob/master/src/handlebars.l
//   - https://github.com/golang/go/blob/master/src/text/template/parse/lex.go

const eof = -1

// lexFunc represents a function that returns the next lexer function.
type lexFunc func(*Lexer) lexFunc

// Lexer is a lexical analyzer.
type Lexer struct {
	input    string     // input to scan
	name     string     // lexer name, used for testing purpose
	tokens   chan Token // channel of scanned tokens
	nextFunc lexFunc    // the next function to execute

	pos   int // current byte position in input string
	line  int // current line position in input string
	width int // size of last rune scanned from input string
	start int // start position of the token we are scanning

	// the shameful contextual properties needed because `nextFunc` is not enough
	closeComment *regexp.Regexp // regexp to scan close of current comment
	rawBlock     bool           // are we parsing a raw block content ?

	// Mustaches detection
	escapedEscapedOpenMustache  string
	escapedOpenMustache         string
	openMustache                string
	closeMustache               string
	closeStripMustache          string
	closeSetDelimMustache       string
	closeUnescapedStripMustache string

	// regular expressions
	rID                  *regexp.Regexp
	rDotID               *regexp.Regexp
	rTrue                *regexp.Regexp
	rFalse               *regexp.Regexp
	rOpenRaw             *regexp.Regexp
	rCloseRaw            *regexp.Regexp
	rOpenEndRaw          *regexp.Regexp
	rOpenEndRawLookAhead *regexp.Regexp
	rOpenUnescaped       *regexp.Regexp
	rCloseUnescaped      *regexp.Regexp
	rOpenBlock           *regexp.Regexp
	rOpenEndBlock        *regexp.Regexp
	rOpenPartial         *regexp.Regexp
	// {{^}} or {{else}}
	rInverse          *regexp.Regexp
	rOpenInverse      *regexp.Regexp
	rOpenInverseChain *regexp.Regexp
	// {{ or {{&
	rOpen            *regexp.Regexp
	rClose           *regexp.Regexp
	rSetDelimOpen    *regexp.Regexp
	rSetDelimClose   *regexp.Regexp
	rOpenBlockParams *regexp.Regexp
	// {{!--  ... --}}
	rOpenCommentDash  *regexp.Regexp
	rCloseCommentDash *regexp.Regexp
	// {{! ... }}
	rOpenComment  *regexp.Regexp
	rCloseComment *regexp.Regexp
}

var (
	lookheadChars        = `[\s` + regexp.QuoteMeta("=~}/)|") + `]`
	literalLookheadChars = `[\s` + regexp.QuoteMeta("~})") + `]`

	// characters not allowed in an identifier
	unallowedIDChars = " \n\t!\"#%&'()*+,./;<=>@[\\]^`{|}~"
)

func (l *Lexer) setDelimiters(openTag string, closeTag string) {
	// Mustaches detection
	l.openMustache = openTag
	l.closeMustache = closeTag
	l.escapedEscapedOpenMustache = "\\\\" + openTag
	l.escapedOpenMustache = "\\" + openTag
	l.closeStripMustache = "~" + closeTag
	l.closeSetDelimMustache = "=" + closeTag
	l.closeUnescapedStripMustache = "}~}}" // TODO: what the heck is this?

	// regular expressions
	l.rID = regexp.MustCompile(`^[^` + regexp.QuoteMeta(unallowedIDChars) + `]+`)
	l.rDotID = regexp.MustCompile(`^\.` + lookheadChars)
	l.rTrue = regexp.MustCompile(`^true` + literalLookheadChars)
	l.rFalse = regexp.MustCompile(`^false` + literalLookheadChars)
	l.rOpenRaw = regexp.MustCompile(`^` + regexp.QuoteMeta(openTag+openTag))
	l.rCloseRaw = regexp.MustCompile(`^` + regexp.QuoteMeta(closeTag+closeTag))
	l.rOpenEndRaw = regexp.MustCompile(`^` + regexp.QuoteMeta(openTag+openTag) + `/`)
	l.rOpenEndRawLookAhead = regexp.MustCompile(regexp.QuoteMeta(openTag+openTag) + `/`)
	l.rOpenUnescaped = regexp.MustCompile(`^` + regexp.QuoteMeta(openTag) + `~?\{`) // TODO: what's up with the training {?
	l.rCloseUnescaped = regexp.MustCompile(`^\}~?` + regexp.QuoteMeta(closeTag))
	l.rOpenBlock = regexp.MustCompile(`^` + regexp.QuoteMeta(openTag) + `~?#`)
	l.rOpenEndBlock = regexp.MustCompile(`^` + regexp.QuoteMeta(openTag) + `~?/`)
	l.rOpenPartial = regexp.MustCompile(`^` + regexp.QuoteMeta(openTag) + `~?>`)
	// {{^}} or {{else}}
	l.rInverse = regexp.MustCompile(`^(` + regexp.QuoteMeta(openTag) + `~?\^\s*~?` + regexp.QuoteMeta(closeTag) + `|` + regexp.QuoteMeta(openTag) + `~?\s*else\s*~?` + regexp.QuoteMeta(closeTag) + `)`)
	l.rOpenInverse = regexp.MustCompile(`^` + regexp.QuoteMeta(openTag) + `~?\^`)
	l.rOpenInverseChain = regexp.MustCompile(`^` + regexp.QuoteMeta(openTag) + `~?\s*else`)
	// {{ or {{&
	l.rOpen = regexp.MustCompile(`^` + regexp.QuoteMeta(openTag) + `~?&?`)
	l.rClose = regexp.MustCompile(`^~?` + regexp.QuoteMeta(closeTag))
	l.rSetDelimOpen = regexp.MustCompile(`^` + regexp.QuoteMeta(openTag) + `=`)
	l.rSetDelimClose = regexp.MustCompile(`^=` + regexp.QuoteMeta(closeTag))
	l.rOpenBlockParams = regexp.MustCompile(`^as\s+\|`)
	// {{!--  ... --}}
	l.rOpenCommentDash = regexp.MustCompile(`^` + regexp.QuoteMeta(openTag) + `~?!--\s*`)
	l.rCloseCommentDash = regexp.MustCompile(`^\s*--~?` + regexp.QuoteMeta(closeTag) + ``)
	// {{! ... }}
	l.rOpenComment = regexp.MustCompile(`^` + regexp.QuoteMeta(openTag) + `~?!\s*`)
	l.rCloseComment = regexp.MustCompile(`^\s*~?` + regexp.QuoteMeta(closeTag) + ``)
}

// Scan scans given input.
//
// Tokens can then be fetched sequentially thanks to NextToken() function on returned lexer.
func Scan(input string) *Lexer {
	return scanWithName(input, "")
}

// scanWithName scans given input, with a name used for testing
//
// Tokens can then be fetched sequentially thanks to NextToken() function on returned lexer.
func scanWithName(input string, name string) *Lexer {
	result := &Lexer{
		input:  input,
		name:   name,
		tokens: make(chan Token),
		line:   1,
	}

	go result.run()

	return result
}

// Collect scans and collect all tokens.
//
// This should be used for debugging purpose only. You should use Scan() and lexer.NextToken() functions instead.
func Collect(input string) []Token {
	var result []Token

	l := Scan(input)
	for {
		token := l.NextToken()
		result = append(result, token)

		if token.Kind == TokenEOF || token.Kind == TokenError {
			break
		}
	}

	return result
}

// NextToken returns the next scanned token.
func (l *Lexer) NextToken() Token {
	result := <-l.tokens

	return result
}

// run starts lexical analysis
func (l *Lexer) run() {
	l.setDelimiters("{{", "}}")

	for l.nextFunc = lexContent; l.nextFunc != nil; {
		l.nextFunc = l.nextFunc(l)
	}
}

// next returns next character from input, or eof of there is nothing left to scan
func (l *Lexer) next() rune {
	if l.pos >= len(l.input) {
		l.width = 0
		return eof
	}

	r, w := utf8.DecodeRuneInString(l.input[l.pos:])
	l.width = w
	l.pos += l.width

	return r
}

func (l *Lexer) produce(kind TokenKind, val string) {
	l.tokens <- Token{kind, val, l.start, l.line}

	// scanning a new token
	l.start = l.pos

	// update line number
	l.line += strings.Count(val, "\n")
}

// emit emits a new scanned token
func (l *Lexer) emit(kind TokenKind) {
	l.produce(kind, l.input[l.start:l.pos])
}

// emitContent emits scanned content
func (l *Lexer) emitContent() {
	if l.pos > l.start {
		l.emit(TokenContent)
	}
}

// emitString emits a scanned string
func (l *Lexer) emitString(delimiter rune) {
	str := l.input[l.start:l.pos]

	// replace escaped delimiters
	str = strings.Replace(str, "\\"+string(delimiter), string(delimiter), -1)

	l.produce(TokenString, str)
}

// peek returns but does not consume the next character in the input
func (l *Lexer) peek() rune {
	r := l.next()
	l.backup()
	return r
}

// backup steps back one character
//
// WARNING: Can only be called once per call of next
func (l *Lexer) backup() {
	l.pos -= l.width
}

// ignoreskips all characters that have been scanned up to current position
func (l *Lexer) ignore() {
	l.start = l.pos
}

// accept scans the next character if it is included in given string
func (l *Lexer) accept(valid string) bool {
	if strings.IndexRune(valid, l.next()) >= 0 {
		return true
	}

	l.backup()

	return false
}

// acceptRun scans all following characters that are part of given string
func (l *Lexer) acceptRun(valid string) {
	for strings.IndexRune(valid, l.next()) >= 0 {
	}

	l.backup()
}

// errorf emits an error token
func (l *Lexer) errorf(format string, args ...interface{}) lexFunc {
	l.tokens <- Token{TokenError, fmt.Sprintf(format, args...), l.start, l.line}
	return nil
}

// isString returns true if content at current scanning position starts with given string
func (l *Lexer) isString(str string) bool {
	return strings.HasPrefix(l.input[l.pos:], str)
}

// findRegexp returns the first string from current scanning position that matches given regular expression
func (l *Lexer) findRegexp(r *regexp.Regexp) string {
	return r.FindString(l.input[l.pos:])
}

// indexRegexp returns the index of the first string from current scanning position that matches given regular expression
//
// It returns -1 if not found
func (l *Lexer) indexRegexp(r *regexp.Regexp) int {
	loc := r.FindStringIndex(l.input[l.pos:])
	if loc == nil {
		return -1
	}
	return loc[0]
}

// lexContent scans content (ie: not between mustaches)
func lexContent(l *Lexer) lexFunc {
	var next lexFunc

	if l.rawBlock {
		if i := l.indexRegexp(l.rOpenEndRawLookAhead); i != -1 {
			// {{{{/
			l.rawBlock = false
			l.pos += i

			next = lexOpenMustache
		} else {
			return l.errorf("Unclosed raw block")
		}
	} else if l.isString(l.escapedEscapedOpenMustache) {
		// \\{{

		// emit content with only one escaped escape
		l.next()
		l.emitContent()

		// ignore second escaped escape
		l.next()
		l.ignore()

		next = lexContent
	} else if l.isString(l.escapedOpenMustache) {
		// \{{
		next = lexEscapedOpenMustache
	} else if str := l.findRegexp(l.rOpenCommentDash); str != "" {
		// {{!--
		l.closeComment = l.rCloseCommentDash

		next = lexComment
	} else if str := l.findRegexp(l.rOpenComment); str != "" {
		// {{!
		l.closeComment = l.rCloseComment

		next = lexComment
	} else if l.isString(l.openMustache) {
		// {{
		next = lexOpenMustache
	}

	if next != nil {
		// emit scanned content
		l.emitContent()

		// scan next token
		return next
	}

	// scan next rune
	if l.next() == eof {
		// emit scanned content
		l.emitContent()

		// this is over
		l.emit(TokenEOF)
		return nil
	}

	// continue content scanning
	return lexContent
}

// lexEscapedOpenMustache scans \{{
func lexEscapedOpenMustache(l *Lexer) lexFunc {
	// ignore escape character
	l.next()
	l.ignore()

	// scan mustaches
	for l.peek() == '{' {
		l.next()
	}

	return lexContent
}

// lexOpenMustache scans {{
func lexOpenMustache(l *Lexer) lexFunc {
	var str string
	var tok TokenKind

	nextFunc := lexExpression

	if str = l.findRegexp(l.rOpenEndRaw); str != "" {
		tok = TokenOpenEndRawBlock
	} else if str = l.findRegexp(l.rOpenRaw); str != "" {
		tok = TokenOpenRawBlock
		l.rawBlock = true
	} else if str = l.findRegexp(l.rOpenUnescaped); str != "" {
		tok = TokenOpenUnescaped
	} else if str = l.findRegexp(l.rOpenBlock); str != "" {
		tok = TokenOpenBlock
	} else if str = l.findRegexp(l.rOpenEndBlock); str != "" {
		tok = TokenOpenEndBlock
	} else if str = l.findRegexp(l.rOpenPartial); str != "" {
		tok = TokenOpenPartial
	} else if str = l.findRegexp(l.rInverse); str != "" {
		tok = TokenInverse
		nextFunc = lexContent
	} else if str = l.findRegexp(l.rOpenInverse); str != "" {
		tok = TokenOpenInverse
	} else if str = l.findRegexp(l.rOpenInverseChain); str != "" {
		tok = TokenOpenInverseChain
	} else if str = l.findRegexp(l.rSetDelimOpen); str != "" {
		l.pos += len(str)
		l.ignore()
		return lexDelimiterAssignment
	} else if str = l.findRegexp(l.rOpen); str != "" {
		tok = TokenOpen
	} else {
		// this is rotten
		panic("Current pos MUST be an opening mustache")
	}

	l.pos += len(str)
	l.emit(tok)

	return nextFunc
}

// lexCloseMustache scans }} or ~}}
func lexCloseMustache(l *Lexer) lexFunc {
	var str string
	var tok TokenKind

	if str = l.findRegexp(l.rCloseRaw); str != "" {
		// }}}}
		tok = TokenCloseRawBlock
	} else if str = l.findRegexp(l.rCloseUnescaped); str != "" {
		// }}}
		tok = TokenCloseUnescaped
	} else if str = l.findRegexp(l.rClose); str != "" {
		// }}
		tok = TokenClose
	} else {
		// this is rotten
		panic("Current pos MUST be a closing mustache")
	}

	l.pos += len(str)
	l.emit(tok)

	return lexContent
}

func lexDelimiterAssignment(l *Lexer) lexFunc {
	// Skip any whitespace
	for isIgnorable(l.peek()) {
		l.next()
	}
	l.ignore()

	newOpenTag := l.findRegexp(regexp.MustCompile(`[^\s]+`))
	l.pos += len(newOpenTag)
	l.ignore()

	for isIgnorable(l.peek()) {
		l.next()
	}
	l.ignore()

	newCloseTag := l.findRegexp(regexp.MustCompile(`[^=\s]+`))
	l.pos += len(newCloseTag)
	l.ignore()

	for isIgnorable(l.peek()) {
		l.next()
	}
	l.ignore()

	oldCloseTag := l.findRegexp(l.rSetDelimClose)
	if oldCloseTag == "" {
		return l.errorf("Expected closeDelimiter tag")
	}
	l.pos += len(oldCloseTag)
	l.ignore()

	l.setDelimiters(newOpenTag, newCloseTag)

	return lexContent
}

// lexExpression scans inside mustaches
func lexExpression(l *Lexer) lexFunc {
	// search close mustache delimiter
	if l.isString(l.closeMustache) || l.isString(l.closeSetDelimMustache) || l.isString(l.closeStripMustache) || l.isString(l.closeUnescapedStripMustache) {
		return lexCloseMustache
	}

	// search some patterns before advancing scanning position

	// "as |"
	if str := l.findRegexp(l.rOpenBlockParams); str != "" {
		l.pos += len(str)
		l.emit(TokenOpenBlockParams)
		return lexExpression
	}

	// ..
	if l.isString("..") {
		l.pos += len("..")
		l.emit(TokenID)
		return lexExpression
	}

	// .
	if str := l.findRegexp(l.rDotID); str != "" {
		l.pos += len(".")
		l.emit(TokenID)
		return lexExpression
	}

	// true
	if str := l.findRegexp(l.rTrue); str != "" {
		l.pos += len("true")
		l.emit(TokenBoolean)
		return lexExpression
	}

	// false
	if str := l.findRegexp(l.rFalse); str != "" {
		l.pos += len("false")
		l.emit(TokenBoolean)
		return lexExpression
	}

	// let's scan next character
	switch r := l.next(); {
	case r == eof:
		return l.errorf("Unclosed expression")
	case isIgnorable(r):
		return lexIgnorable
	case r == '(':
		l.emit(TokenOpenSexpr)
	case r == ')':
		l.emit(TokenCloseSexpr)
	case r == '=':
		l.emit(TokenEquals)
	case r == '@':
		l.emit(TokenData)
	case r == '"' || r == '\'':
		l.backup()
		return lexString
	case r == '/' || r == '.':
		l.emit(TokenSep)
	case r == '|':
		l.emit(TokenCloseBlockParams)
	case r == '+' || r == '-' || (r >= '0' && r <= '9'):
		l.backup()
		return lexNumber
	case r == '[':
		return lexPathLiteral
	case strings.IndexRune(unallowedIDChars, r) < 0:
		l.backup()
		return lexIdentifier
	default:
		return l.errorf("Unexpected character in expression: '%c'", r)
	}

	return lexExpression
}

// lexComment scans {{!-- or {{!
func lexComment(l *Lexer) lexFunc {
	if str := l.findRegexp(l.closeComment); str != "" {
		l.pos += len(str)
		l.emit(TokenComment)

		return lexContent
	}

	if r := l.next(); r == eof {
		return l.errorf("Unclosed comment")
	}

	return lexComment
}

// lexIgnorable scans all following ignorable characters
func lexIgnorable(l *Lexer) lexFunc {
	for isIgnorable(l.peek()) {
		l.next()
	}
	l.ignore()

	return lexExpression
}

// lexString scans a string
func lexString(l *Lexer) lexFunc {
	// get string delimiter
	delim := l.next()
	var prev rune

	// ignore delimiter
	l.ignore()

	for {
		r := l.next()
		if r == eof || r == '\n' {
			return l.errorf("Unterminated string")
		}

		if (r == delim) && (prev != '\\') {
			break
		}

		prev = r
	}

	// remove end delimiter
	l.backup()

	// emit string
	l.emitString(delim)

	// skip end delimiter
	l.next()
	l.ignore()

	return lexExpression
}

// lexNumber scans a number: decimal, octal, hex, float, or imaginary. This
// isn't a perfect number scanner - for instance it accepts "." and "0x0.2"
// and "089" - but when it's wrong the input is invalid and the parser (via
// strconv) will notice.
//
// NOTE: borrowed from https://github.com/golang/go/tree/master/src/text/template/parse/lex.go
func lexNumber(l *Lexer) lexFunc {
	if !l.scanNumber() {
		return l.errorf("bad number syntax: %q", l.input[l.start:l.pos])
	}
	if sign := l.peek(); sign == '+' || sign == '-' {
		// Complex: 1+2i. No spaces, must end in 'i'.
		if !l.scanNumber() || l.input[l.pos-1] != 'i' {
			return l.errorf("bad number syntax: %q", l.input[l.start:l.pos])
		}
		l.emit(TokenNumber)
	} else {
		l.emit(TokenNumber)
	}
	return lexExpression
}

// scanNumber scans a number
//
// NOTE: borrowed from https://github.com/golang/go/tree/master/src/text/template/parse/lex.go
func (l *Lexer) scanNumber() bool {
	// Optional leading sign.
	l.accept("+-")

	// Is it hex?
	digits := "0123456789"

	if l.accept("0") && l.accept("xX") {
		digits = "0123456789abcdefABCDEF"
	}

	l.acceptRun(digits)

	if l.accept(".") {
		l.acceptRun(digits)
	}

	if l.accept("eE") {
		l.accept("+-")
		l.acceptRun("0123456789")
	}

	// Is it imaginary?
	l.accept("i")

	// Next thing mustn't be alphanumeric.
	if isAlphaNumeric(l.peek()) {
		l.next()
		return false
	}

	return true
}

// lexIdentifier scans an ID
func lexIdentifier(l *Lexer) lexFunc {
	str := l.findRegexp(l.rID)
	if len(str) == 0 {
		// this is rotten
		panic("Identifier expected")
	}

	l.pos += len(str)
	l.emit(TokenID)

	return lexExpression
}

// lexPathLiteral scans an [ID]
func lexPathLiteral(l *Lexer) lexFunc {
	for {
		r := l.next()
		if r == eof || r == '\n' {
			return l.errorf("Unterminated path literal")
		}

		if r == ']' {
			break
		}
	}

	l.emit(TokenID)

	return lexExpression
}

// isIgnorable returns true if given character is ignorable (ie. whitespace of line feed)
func isIgnorable(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n'
}

// isAlphaNumeric reports whether r is an alphabetic, digit, or underscore.
//
// NOTE borrowed from https://github.com/golang/go/tree/master/src/text/template/parse/lex.go
func isAlphaNumeric(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
