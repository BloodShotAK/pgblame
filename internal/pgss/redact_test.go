package pgss

import "testing"

func TestRedact(t *testing.T) {
	cases := map[string]string{
		`CREATE ROLE app LOGIN PASSWORD 'hunter2'`:                              `CREATE ROLE app LOGIN PASSWORD '***'`,
		`alter user app with encrypted password 'it''s secret'`:                 `alter user app with encrypted password '***'`,
		`ALTER ROLE app PASSWORD E'esc\'aped'`:                                  `ALTER ROLE app PASSWORD '***'`,
		`ALTER ROLE app PASSWORD $pw$dollar'quoted$pw$ VALID UNTIL`:             `ALTER ROLE app PASSWORD '***' VALID UNTIL`,
		`CREATE USER MAPPING FOR app SERVER s OPTIONS (user 'u', password 'p')`: `CREATE USER MAPPING FOR app SERVER s OPTIONS (user 'u', password '***')`,
		`SELECT dblink_connect('host=db user=u password=s3cret dbname=x')`:      `SELECT dblink_connect('host=db user=u password=*** dbname=x')`,
		`SELECT * FROM users WHERE password_hash = $1`:                          `SELECT * FROM users WHERE password_hash = $1`,
		`UPDATE accounts SET password = $1 WHERE id = $2`:                       `UPDATE accounts SET password = $1 WHERE id = $2`,
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}
