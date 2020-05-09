package mapping

import (
	"fmt"
	"strings"

	"github.com/Jeffail/benthos/v3/lib/bloblang/x/parser"
	"github.com/Jeffail/benthos/v3/lib/bloblang/x/query"
	"github.com/Jeffail/benthos/v3/lib/types"
	"github.com/Jeffail/gabs/v2"
	"golang.org/x/xerrors"
)

//------------------------------------------------------------------------------

// Message is an interface type to be given to a query function, it allows the
// function to resolve fields and metadata from a message.
type Message interface {
	Get(p int) types.Part
	Len() int
}

//------------------------------------------------------------------------------

type mappingStatement struct {
	line       int
	assignment Assignment
	query      query.Function
}

// Executor is a parsed bloblang mapping that can be executed on a Benthos
// message.
type Executor struct {
	maps       map[string]query.Function
	statements []mappingStatement
}

// MapPart executes the bloblang mapping on a particular message index of a
// batch. The message is parsed as a JSON document in order to provide the
// mapping context. Returns an error if any stage of the mapping fails to
// execute.
//
// Note that the message is mutated in situ and therefore the contents may be
// modified even if an error is returned.
func (e *Executor) MapPart(index int, msg Message) error {
	vars := map[string]interface{}{}
	meta := msg.Get(index).Metadata()

	var valuePtr *interface{}
	if jObj, err := msg.Get(index).JSON(); err == nil {
		valuePtr = &jObj
	}

	var newObj interface{} = query.Nothing(nil)
	for _, stmt := range e.statements {
		res, err := stmt.query.Exec(query.FunctionContext{
			Maps:  e.maps,
			Value: valuePtr,
			Vars:  vars,
			Index: index,
			Msg:   msg,
		})
		if err != nil {
			return xerrors.Errorf("failed to execute mapping assignment at line %v: %v", stmt.line, err)
		}
		if err = stmt.assignment.Apply(res, AssignmentContext{
			Maps:  e.maps,
			Vars:  vars,
			Meta:  meta,
			Value: &newObj,
		}); err != nil {
			return xerrors.Errorf("failed to assign mapping result at line %v: %v", stmt.line, err)
		}
	}

	if _, notMapped := newObj.(query.Nothing); !notMapped {
		if err := msg.Get(index).SetJSON(newObj); err != nil {
			return xerrors.Errorf("failed to set result of mapping: %w", err)
		}
	}
	return nil
}

// Exec this function with a context struct.
func (e *Executor) Exec(ctx query.FunctionContext) (interface{}, error) {
	meta := ctx.Msg.Get(ctx.Index).Metadata()

	var newObj interface{} = query.Nothing(nil)
	for _, stmt := range e.statements {
		res, err := stmt.query.Exec(ctx)
		if err != nil {
			return nil, xerrors.Errorf("failed to execute mapping assignment at line %v: %v", stmt.line, err)
		}
		if err = stmt.assignment.Apply(res, AssignmentContext{
			Maps:  e.maps,
			Vars:  ctx.Vars,
			Meta:  meta,
			Value: &newObj,
		}); err != nil {
			return nil, xerrors.Errorf("failed to assign mapping result at line %v: %v", stmt.line, err)
		}
	}

	return newObj, nil
}

// ToBytes executes this function for a message of a batch and returns the
// result marshalled into a byte slice.
func (e *Executor) ToBytes(ctx query.FunctionContext) []byte {
	v, err := e.Exec(ctx)
	if err != nil {
		if rec, ok := err.(*query.ErrRecoverable); ok {
			return query.IToBytes(rec.Recovered)
		}
		return nil
	}
	return query.IToBytes(v)
}

// ToString executes this function for a message of a batch and returns the
// result marshalled into a string.
func (e *Executor) ToString(ctx query.FunctionContext) string {
	v, err := e.Exec(ctx)
	if err != nil {
		if rec, ok := err.(*query.ErrRecoverable); ok {
			return query.IToString(rec.Recovered)
		}
		return ""
	}
	return query.IToString(v)
}

// NewExecutor parses a bloblang mapping and returns an executor to run it, or
// an error if the parsing fails.
func NewExecutor(mapping string) (*Executor, error) {
	res := ParseExecutor([]rune(mapping))
	if res.Err != nil {
		return nil, xerrors.Errorf("failed to parse mapping: %w", res.Err)
	}

	return res.Result.(*Executor), nil
}

//------------------------------------------------------------------------------'

type mappingParseError struct {
	filename string
	line     int
	column   int
	err      error
}

func (e *mappingParseError) Error() string {
	errStr := fmt.Sprintf("line %v char %v: %v", e.line+1, e.column+1, e.err)
	if len(e.filename) > 0 {
		return fmt.Sprintf("file %v: %v", e.filename, errStr)
	}
	return errStr
}

func getLineCol(lines []int, char int) (int, int) {
	line, column := 0, char
	for i, index := range lines {
		if index > char {
			break
		}
		line = i + 1
		column = char - index
	}
	return line, column
}

func wrapParserErr(lines []int, filename string, err error) error {
	if p, ok := err.(parser.PositionalError); ok {
		line, column := getLineCol(lines, p.Position)
		return &mappingParseError{
			filename: filename,
			line:     line,
			column:   column,
			err:      p.Err,
		}
	}
	return err
}

// ParseExecutor implements parser.Type and parses an input into a bloblang
// mapping executor. Returns an *Executor unless a parsing error occurs.
func ParseExecutor(input []rune) parser.Result {
	maps := map[string]query.Function{}
	statements := []mappingStatement{}

	var i int
	var lineIndexes []int
	for _, l := range strings.Split(string(input), "\n") {
		i = i + len(l) + 1
		lineIndexes = append(lineIndexes, i)
	}

	newline := parser.NewlineAllowComment()
	whitespace := parser.SpacesAndTabs()
	allWhitespace := parser.DiscardAll(parser.AnyOf(whitespace, newline))

	statement := parser.AnyOf(
		mapParser(maps),
		letStatementParser(),
		metaStatementParser(),
		plainMappingStatementParser(),
	)

	res := allWhitespace(input)

	i = len(input) - len(res.Remaining)
	res = statement(res.Remaining)
	if res.Err != nil {
		res.Err = wrapParserErr(lineIndexes, "", parser.ErrAtPosition(i, res.Err))
		return res
	}
	if mStmt, ok := res.Result.(mappingStatement); ok {
		mStmt.line, _ = getLineCol(lineIndexes, i)
		statements = append(statements, mStmt)
	}

	for {
		res = parser.Discard(whitespace)(res.Remaining)
		if len(res.Remaining) == 0 {
			break
		}

		i = len(input) - len(res.Remaining)
		if res = newline(res.Remaining); res.Err != nil {
			return parser.Result{
				Err:       wrapParserErr(lineIndexes, "", parser.ErrAtPosition(i, res.Err)),
				Remaining: input,
			}
		}

		res = allWhitespace(res.Remaining)
		if len(res.Remaining) == 0 {
			break
		}

		i = len(input) - len(res.Remaining)
		if res = statement(res.Remaining); res.Err != nil {
			return parser.Result{
				Err:       wrapParserErr(lineIndexes, "", parser.ErrAtPosition(i, res.Err)),
				Remaining: input,
			}
		}
		if mStmt, ok := res.Result.(mappingStatement); ok {
			mStmt.line, _ = getLineCol(lineIndexes, i)
			statements = append(statements, mStmt)
		}
	}

	return parser.Result{
		Remaining: res.Remaining,
		Result: &Executor{
			maps, statements,
		},
	}
}

//------------------------------------------------------------------------------

func pathLiteralParser() parser.Type {
	return parser.JoinStringSliceResult(
		parser.AllOf(
			parser.AnyOf(
				parser.InRange('a', 'z'),
				parser.InRange('A', 'Z'),
				parser.InRange('0', '9'),
				parser.InRange('*', '-'),
				parser.Char('.'),
				parser.Char('_'),
				parser.Char('~'),
			),
		),
	)
}

func mapParser(maps map[string]query.Function) parser.Type {
	newline := parser.NewlineAllowComment()
	whitespace := parser.SpacesAndTabs()
	allWhitespace := parser.DiscardAll(parser.AnyOf(whitespace, newline))

	p := parser.Sequence(
		parser.Match("map"),
		whitespace,
		// Prevents a missing path from being captured by the next parser
		parser.MustBe(
			parser.InterceptExpectedError(
				parser.AnyOf(
					parser.QuotedString(),
					pathLiteralParser(),
				),
				"map-name",
			),
		),
		parser.SpacesAndTabs(),
		parser.DelimitedPattern(
			parser.Sequence(
				parser.Char('{'),
				allWhitespace,
			),
			parser.AnyOf(
				letStatementParser(),
				metaStatementParser(),
				plainMappingStatementParser(),
			),
			parser.Sequence(
				parser.Discard(whitespace),
				newline,
				allWhitespace,
			),
			parser.Sequence(
				allWhitespace,
				parser.Char('}'),
			),
			true, false,
		),
	)

	return func(input []rune) parser.Result {
		res := p(input)
		if res.Err != nil {
			return res
		}

		seqSlice := res.Result.([]interface{})
		ident := seqSlice[2].(string)
		stmtSlice := seqSlice[4].([]interface{})

		if _, exists := maps[ident]; exists {
			return parser.Result{
				Err:       fmt.Errorf("map name collision: %v", ident),
				Remaining: input,
			}
		}

		statements := make([]mappingStatement, len(stmtSlice))
		for i, v := range stmtSlice {
			statements[i] = v.(mappingStatement)
		}

		maps[ident] = &Executor{maps, statements}

		return parser.Result{
			Result:    ident,
			Remaining: res.Remaining,
		}
	}
}

func letStatementParser() parser.Type {
	p := parser.Sequence(
		parser.Match("let"),
		parser.SpacesAndTabs(),
		// Prevents a missing path from being captured by the next parser
		parser.MustBe(
			parser.InterceptExpectedError(
				parser.AnyOf(
					parser.QuotedString(),
					pathLiteralParser(),
				),
				"variable-name",
			),
		),
		parser.SpacesAndTabs(),
		parser.Char('='),
		parser.SpacesAndTabs(),
		query.Parse,
	)

	return func(input []rune) parser.Result {
		res := p(input)
		if res.Err != nil {
			return res
		}
		resSlice := res.Result.([]interface{})
		return parser.Result{
			Result: mappingStatement{
				assignment: &varAssignment{
					Name: resSlice[2].(string),
				},
				query: resSlice[6].(query.Function),
			},
			Remaining: res.Remaining,
		}
	}
}

func metaStatementParser() parser.Type {
	p := parser.Sequence(
		parser.Match("meta"),
		parser.SpacesAndTabs(),
		parser.Optional(parser.AnyOf(
			parser.QuotedString(),
			pathLiteralParser(),
		)),
		parser.Optional(parser.SpacesAndTabs()),
		parser.Char('='),
		parser.SpacesAndTabs(),
		query.Parse,
	)

	return func(input []rune) parser.Result {
		res := p(input)
		if res.Err != nil {
			return res
		}
		resSlice := res.Result.([]interface{})

		var keyPtr *string
		if key, set := resSlice[2].(string); set {
			keyPtr = &key
		}

		return parser.Result{
			Result: mappingStatement{
				assignment: &metaAssignment{Key: keyPtr},
				query:      resSlice[6].(query.Function),
			},
			Remaining: res.Remaining,
		}
	}
}

func plainMappingStatementParser() parser.Type {
	p := parser.Sequence(
		parser.InterceptExpectedError(
			parser.AnyOf(
				parser.QuotedString(),
				pathLiteralParser(),
			),
			"target-path",
		),
		parser.SpacesAndTabs(),
		parser.Char('='),
		parser.SpacesAndTabs(),
		query.Parse,
	)

	return func(input []rune) parser.Result {
		res := p(input)
		if res.Err != nil {
			return res
		}
		resSlice := res.Result.([]interface{})
		path := gabs.DotPathToSlice(resSlice[0].(string))
		if len(path) > 0 && path[0] == "root" {
			path = path[1:]
		}
		return parser.Result{
			Result: mappingStatement{
				assignment: &jsonAssignment{
					Path: path,
				},
				query: resSlice[4].(query.Function),
			},
			Remaining: res.Remaining,
		}
	}
}

//------------------------------------------------------------------------------
