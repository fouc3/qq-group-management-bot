package store

import (
	"fmt"
	"strings"
)

// dialect is everything that differs between SQLite and PostgreSQL.
//
// It is one value with one method because that is the whole difference: the
// statements themselves are written to work on both.
type dialect struct {
	name string
	// postgres asks for the placeholders to be rewritten.
	postgres bool
}

// rewrite turns a canonical query into one this dialect accepts.
//
// Canonical queries are written the SQLite way, with ?. PostgreSQL rejects ?, so
// a placeholder becomes $1, $2 and so on. Only a ? outside a quoted run is a
// placeholder: a ? inside a string literal is data, and rewriting it would bind
// the wrong values or fail to parse.
//
// Queries in this package carry no SQL comments, so none are skipped here. A
// future query that needs one should have its comment removed instead: a rule
// that is easy to keep is worth more than a parser that is easy to get wrong.
func (d dialect) rewrite(query string) string {
	if !d.postgres {
		return query
	}
	var out strings.Builder
	out.Grow(len(query) + 8)
	placeholders := 0
	for i := 0; i < len(query); i++ {
		switch current := query[i]; current {
		case '\'', '"':
			quote := current
			out.WriteByte(quote)
			i++
			for i < len(query) {
				out.WriteByte(query[i])
				if query[i] == quote {
					// A doubled quote is an escaped quote inside the literal.
					if i+1 < len(query) && query[i+1] == quote {
						out.WriteByte(query[i+1])
						i += 2
						continue
					}
					break
				}
				i++
			}
		case '?':
			placeholders++
			out.WriteString(fmt.Sprintf("$%d", placeholders))
		default:
			out.WriteByte(current)
		}
	}
	return out.String()
}
