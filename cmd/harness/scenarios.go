package main

import (
	"math/rand/v2"

	"github.com/BloodShotAK/pgblame/internal/detect"
)

const dbName = "pgblame_harness"

var schema = []string{
	`CREATE TABLE customers (id int PRIMARY KEY, name text NOT NULL, visits int NOT NULL DEFAULT 0)`,
	`INSERT INTO customers (id, name) SELECT i, 'customer ' || i FROM generate_series(1, 50000) i`,
	`CREATE TABLE orders (id bigserial PRIMARY KEY, customer_id int NOT NULL, amount numeric NOT NULL, status text NOT NULL)`,
	`INSERT INTO orders (customer_id, amount, status)
	 SELECT i % 20000 + 1, round((random() * 100)::numeric, 2), 'open' FROM generate_series(1, 200000) i`,
	`CREATE INDEX orders_customer_idx ON orders (customer_id)`,
	`VACUUM ANALYZE customers`,
	`VACUUM ANALYZE orders`,
	`GRANT SELECT, UPDATE ON customers, orders TO harness_app`,
}

// Each query carries its name in a comment, which pg_stat_statements keeps
// in the stored text, so results can be matched back to it.
type query struct {
	name string
	sql  string
	arg  func(*rand.Rand) int64
}

var queries = []query{
	{"orders_by_customer", `SELECT /* harness:orders_by_customer */ id, amount FROM orders WHERE customer_id = $1`,
		func(r *rand.Rand) int64 { return r.Int64N(200) + 1 }},
	{"customer_by_id", `SELECT /* harness:customer_by_id */ name, visits FROM customers WHERE id = $1`,
		func(r *rand.Rand) int64 { return r.Int64N(50000) + 1 }},
	{"customer_visit", `UPDATE /* harness:customer_visit */ customers SET visits = visits + 1 WHERE id = $1`,
		func(r *rand.Rand) int64 { return r.Int64N(50000) + 1 }},
	{"orders_range", `SELECT /* harness:orders_range */ sum(amount) FROM orders WHERE id BETWEEN $1 AND $1 + 50`,
		func(r *rand.Rand) int64 { return r.Int64N(190000) + 1 }},
}

type scenario struct {
	name    string
	inject  []string
	restore []string
	// Settings changed with ALTER DATABASE only reach new sessions.
	reconnect bool
	expect    map[string]detect.Kind
}

var scenarios = []scenario{
	{
		name:   "control",
		expect: map[string]detect.Kind{},
	},
	{
		name:    "drop_index",
		inject:  []string{`DROP INDEX orders_customer_idx`},
		restore: []string{`CREATE INDEX orders_customer_idx ON orders (customer_id)`},
		expect:  map[string]detect.Kind{"orders_by_customer": detect.MoreWork},
	},
	{
		name: "planner_setting",
		inject: []string{
			`ALTER DATABASE ` + dbName + ` SET enable_indexscan = off`,
			`ALTER DATABASE ` + dbName + ` SET enable_bitmapscan = off`,
		},
		restore:   []string{`ALTER DATABASE ` + dbName + ` RESET ALL`},
		reconnect: true,
		expect: map[string]detect.Kind{
			"orders_by_customer": detect.MoreWork,
			"customer_by_id":     detect.MoreWork,
			"customer_visit":     detect.MoreWork,
			"orders_range":       detect.MoreWork,
		},
	},
	{
		name: "data_growth",
		inject: []string{
			`INSERT INTO orders (customer_id, amount, status)
			 SELECT c, round((random() * 100)::numeric, 2), 'open' FROM generate_series(1, 200) c, generate_series(1, 100)`,
			`ANALYZE orders`,
		},
		restore: []string{`DELETE FROM orders WHERE id > 200000`, `VACUUM ANALYZE orders`},
		expect:  map[string]detect.Kind{"orders_by_customer": detect.MoreRows},
	},
}
