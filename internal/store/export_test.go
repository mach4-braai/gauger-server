package store

import "time"

// ResourcesStatements returns the SQL and arguments of every query that
// Resources runs over samples.
func ResourcesStatements(f Filter, lo time.Time, by, step string) (sqls []string, args [][]any) {
	for _, q := range resourcesStatements(f, lo, by, step) {
		sqls = append(sqls, q.sql)
		args = append(args, q.args)
	}
	return sqls, args
}

// ResourcesUncounted is the interface list the traffic queries leave out.
var ResourcesUncounted = resourcesUncounted
