package server

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"wpgo/internal/backupcrypt"
	"wpgo/internal/cli"
	"wpgo/internal/config"
)

// `backup remote …` and `backup decrypt`: the remote storage of Backup Manager (backup_remote.go)
// from the command line. Secrets are never printed; they are read from files (or stdin with "-")
// rather than flags, so they stay out of the shell history and the process list.

var backupRemoteListFields = []string{"name", "size", "uploaded", "local"}

// cliReadSecret reads a secret from a file or, with "-", from stdin (first line, trimmed).
func cliReadSecret(c *cli.Ctx, file string) (string, error) {
	b, err := c.ReadInput(file, os.ReadFile)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimRight(line, "\r"), nil
}

func registerBackupRemoteCLI(r *cli.Registry, e *cliEnv) {
	r.Noun("backup remote", "Encrypted copies of backups in an S3-compatible bucket (AWS S3, R2, B2, MinIO …).")
	r.Add(cli.Command{
		Path:  "backup remote status",
		Short: "Show the remote storage settings (without secrets).",
		Long: "Prints the S3-compatible target of Backup Manager. The secret key and the encryption passphrase\n" +
			"are only reported as set or not set. Fields set by PURSAE_BACKUP_S3_* / PURSAE_BACKUP_PASSPHRASE\n" +
			"name their variable.",
		Run: func(c *cli.Ctx) error {
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			st := s.bmgrRemote()
			locked := bmgrRemoteLocked()
			src := func(field string) string {
				if v := locked[field]; v != "" {
					return v
				}
				return "settings"
			}
			problem := ""
			if err := st.problem(); err != nil && st.configured() {
				problem = err.Error()
			}
			return c.PrintItem(cli.Row{"configured": st.configured(), "endpoint": st.Endpoint, "region": defaultStr(st.Region, "us-east-1"),
				"bucket": st.Bucket, "prefix": st.Prefix, "path_style": st.PathStyle, "access_key": st.AccessKey != "",
				"secret_key": st.SecretKey != "", "passphrase": st.Passphrase != "", "auto": st.Auto, "keep": st.Keep,
				"endpoint_from": src("endpoint"), "secret_key_from": src("secret_key"), "passphrase_from": src("passphrase"),
				"problem": problem},
				[]string{"configured", "endpoint", "region", "bucket", "prefix", "path_style", "access_key", "secret_key", "passphrase",
					"auto", "keep", "endpoint_from", "secret_key_from", "passphrase_from", "problem"})
		},
	})

	r.Add(cli.Command{
		Path:  "backup remote set",
		Short: "Change the remote storage settings.",
		Long: "Sets the S3-compatible bucket backups are copied to, encrypted. Only the given flags change.\n" +
			"The secret key and the passphrase are read from a file (\"-\": the first line of stdin) and stored\n" +
			"sealed with the site secret; better still, set PURSAE_BACKUP_S3_SECRET_KEY and\n" +
			"PURSAE_BACKUP_PASSPHRASE in the environment, which keeps them out of the database.\n" +
			"An empty --bucket switches remote storage off.",
		Flags: []cli.Flag{
			{Name: "endpoint", Usage: "S3 API endpoint, e.g. https://s3.eu-central-1.amazonaws.com (https, public address)."},
			{Name: "region", Usage: "Region (empty: us-east-1; Cloudflare R2: auto)."},
			{Name: "bucket", Usage: "Bucket name (empty: remote storage off)."},
			{Name: "prefix", Usage: "Folder (key prefix) in the bucket, e.g. pursae-backups/."},
			{Name: "path-style", Bool: true, Usage: "Path-style requests (MinIO and most self-hosted servers)."},
			{Name: "access-key", Usage: "Access key ID."},
			{Name: "secret-key-file", Usage: "File holding the secret access key (\"-\": stdin)."},
			{Name: "passphrase-file", Usage: "File holding the encryption passphrase, at least 12 characters (\"-\": stdin)."},
			{Name: "auto", Bool: true, Usage: "Upload every new manual and scheduled backup automatically."},
			{Name: "keep", Usage: "Newest backups kept in the bucket (0 keeps all)."},
		},
		Examples: []string{
			"backup remote set --endpoint=https://s3.eu-central-1.amazonaws.com --region=eu-central-1 --bucket=my-backups --prefix=site1/ --access-key=AKIA… --secret-key-file=/run/secrets/s3 --passphrase-file=/run/secrets/backup-pass --auto --keep=14",
			"backup remote set --keep=30",
			"backup remote set --bucket=",
		},
		Run: func(c *cli.Ctx) error {
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			if c.Flag("secret-key-file") == "-" && c.Flag("passphrase-file") == "-" {
				return cli.Usagef("only one of --secret-key-file and --passphrase-file can read stdin")
			}
			st := s.bmgrRemoteStored()
			st.SecretKey, st.Passphrase = "", "" // empty keeps the stored (sealed) values
			if c.Has("endpoint") {
				st.Endpoint = c.Flag("endpoint")
			}
			if c.Has("region") {
				st.Region = c.Flag("region")
			}
			if c.Has("bucket") {
				st.Bucket = c.Flag("bucket")
			}
			if c.Has("prefix") {
				st.Prefix = c.Flag("prefix")
			}
			if c.Has("path-style") {
				st.PathStyle = c.Bool("path-style")
			}
			if c.Has("access-key") {
				st.AccessKey = c.Flag("access-key")
			}
			if c.Has("auto") {
				st.Auto = c.Bool("auto")
			}
			if c.Has("keep") {
				if st.Keep, err = c.Int("keep", 0); err != nil {
					return err
				}
			}
			if f := c.Flag("secret-key-file"); f != "" {
				if st.SecretKey, err = cliReadSecret(c, f); err != nil {
					return err
				}
				if st.SecretKey == "" {
					return cli.Errorf("the secret key file is empty")
				}
			}
			if f := c.Flag("passphrase-file"); f != "" {
				if st.Passphrase, err = cliReadSecret(c, f); err != nil {
					return err
				}
				if st.Passphrase == "" {
					return cli.Errorf("the passphrase file is empty")
				}
			}
			if err := s.bmgrRemoteSave(c.Context, st, false); err != nil {
				return err
			}
			for field, env := range bmgrRemoteLocked() {
				c.Warning("%s comes from %s; the environment wins over the saved value.", field, env)
			}
			c.Success("Remote storage settings saved.")
			return nil
		},
	})

	r.Add(cli.Command{
		Path:  "backup remote test",
		Short: "Check the connection: write, list, read back and delete an encrypted test file.",
		Run: func(c *cli.Ctx) error {
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			steps, err := s.bmgrRemoteTest(c.Context)
			for _, st := range steps {
				c.Log("%s: ok", st)
			}
			if err != nil {
				return err
			}
			c.Success("The bucket works.")
			return nil
		},
	})

	r.Add(cli.Command{
		Path:  "backup remote list",
		Short: "List the encrypted backups in the bucket, newest first.",
		Examples: []string{
			"backup remote list",
			"backup remote list --format=json",
		},
		Run: func(c *cli.Ctx) error {
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			st := s.bmgrRemote()
			if err := st.problem(); err != nil {
				return err
			}
			list, err := bmgrRemoteList(c.Context, bmgrRemoteClient(st), st)
			if err != nil {
				return err
			}
			var out []cli.Row
			for _, o := range list {
				out = append(out, cli.Row{"name": o.Name, "key": o.Key, "size": bmgrHuman(o.Size), "bytes": o.Size, "uploaded": o.Modified,
					"local": s.bmgrPath(o.LocalName()) != ""})
			}
			return c.Print(out, cli.View{Fields: backupRemoteListFields, ID: "name"})
		},
	})

	r.Add(cli.Command{
		Path:  "backup remote push",
		Short: "Encrypt a stored backup and upload it to the bucket.",
		Long: "Uploads an archive of the backups directory (from `backup list`), encrypted, as a stream (multipart\n" +
			"upload for large files), then deletes the oldest remote backups beyond the retention count.",
		Args:     []cli.Arg{{Name: "file", Usage: "Archive name (from backup list)."}},
		Examples: []string{"backup remote push backup-20260101-120000-abcd1234.zip"},
		Run: func(c *cli.Ctx) error {
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			name := c.Arg(0)
			if s.bmgrPath(name) == "" {
				return cli.Errorf("backup '%s' not found in %s", name, s.bmgrDir())
			}
			key, err := s.bmgrRemoteUpload(c.Context, name, func(f string, a ...any) { c.Log(f, a...) }, nil)
			if err != nil {
				return err
			}
			c.Success("Uploaded: %s", key)
			return nil
		},
	})

	r.Add(cli.Command{
		Path:  "backup remote pull",
		Short: "Download and decrypt a remote backup into the backups directory.",
		Long: "Fetches <name> (from `backup remote list`), decrypts it while it downloads and checks the archive;\n" +
			"then restore it with `backup restore <archive>`. Works before the site is installed, with the\n" +
			"bucket and passphrase given by PURSAE_BACKUP_S3_* and PURSAE_BACKUP_PASSPHRASE (disaster recovery\n" +
			"on a new server).",
		Args:  []cli.Arg{{Name: "name", Usage: "Remote backup name (….zip.enc)."}},
		Flags: []cli.Flag{{Name: "passphrase-file", Usage: "Passphrase of this backup, if it differs from the configured one (\"-\": stdin)."}},
		Examples: []string{
			"backup remote pull backup-20260101-120000-abcd1234.zip.enc",
			"backup remote pull old-20250101-030000-abcd1234.zip.enc --passphrase-file=./old-pass.txt",
		},
		Run: func(c *cli.Ctx) error {
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			pass := ""
			if f := c.Flag("passphrase-file"); f != "" {
				if pass, err = cliReadSecret(c, f); err != nil {
					return err
				}
			}
			local, err := s.bmgrRemoteFetch(c.Context, c.Arg(0), pass, func(f string, a ...any) { c.Log(f, a...) }, nil)
			if err != nil {
				return err
			}
			c.Success("Ready to restore: %s (pursae backup restore %s)", filepath.Join(s.bmgrDir(), local), local)
			return nil
		},
	})

	r.Add(cli.Command{
		Path:     "backup remote delete",
		Short:    "Delete encrypted backups from the bucket.",
		Args:     []cli.Arg{{Name: "name", Usage: "Remote backup name (from backup remote list).", Repeat: true}},
		Examples: []string{"backup remote delete backup-20260101-120000-abcd1234.zip.enc --yes"},
		Run: func(c *cli.Ctx) error {
			s, err := e.Server(c.Context)
			if err != nil {
				return err
			}
			for _, n := range c.Args {
				if !bmgrRemoteNameRe.MatchString(n) {
					return cli.Errorf("'%s' is not an encrypted backup name (….zip.enc)", n)
				}
			}
			if err := c.Confirm("Delete %d backup(s) from the bucket permanently?", len(c.Args)); err != nil {
				return err
			}
			for _, n := range c.Args {
				if err := s.bmgrRemoteDelete(c.Context, n); err != nil {
					return err
				}
				c.Log("Deleted %s", n)
			}
			c.Success("Deleted %d remote backup(s).", len(c.Args))
			return nil
		},
	})

	r.Add(cli.Command{
		Path:  "backup decrypt",
		Short: "Decrypt an encrypted backup file (….zip.enc) into a .zip.",
		Long: "For a backup downloaded from the bucket with any S3 tool. The passphrase comes from\n" +
			"--passphrase-file, else PURSAE_BACKUP_PASSPHRASE, else the site's remote storage settings.\n" +
			"Nothing is written when the passphrase is wrong or the file was changed.",
		Args:  []cli.Arg{{Name: "file", Usage: "Encrypted file."}},
		Flags: []cli.Flag{{Name: "out", Usage: "Output file (default: the input without .enc)."}, {Name: "passphrase-file", Usage: "File holding the passphrase (\"-\": stdin)."}},
		Examples: []string{
			"backup decrypt ./backup-20260101-120000-abcd1234.zip.enc --passphrase-file=./pass.txt",
		},
		Run: func(c *cli.Ctx) error {
			in := c.Arg(0)
			out := c.Flag("out")
			if out == "" {
				out = strings.TrimSuffix(in, backupcrypt.Suffix)
				if out == in {
					out = in + ".zip"
				}
			}
			pass := config.Env("BACKUP_PASSPHRASE")
			if f := c.Flag("passphrase-file"); f != "" {
				var err error
				if pass, err = cliReadSecret(c, f); err != nil {
					return err
				}
			}
			if pass == "" {
				s, err := e.Server(c.Context)
				if err != nil {
					return err
				}
				pass = s.bmgrRemote().Passphrase
			}
			if pass == "" {
				return cli.Errorf("no passphrase: use --passphrase-file or set %s", config.EnvName("BACKUP_PASSPHRASE"))
			}
			if _, err := os.Stat(out); err == nil {
				return cli.Errorf("%s exists already", out)
			}
			src, err := os.Open(in)
			if err != nil {
				return err
			}
			defer src.Close()
			tmp := out + ".part"
			dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			n, err := backupcrypt.DecryptStream(dst, src, []byte(pass))
			if cerr := dst.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				os.Remove(tmp)
				if errors.Is(err, io.ErrUnexpectedEOF) {
					err = backupcrypt.ErrAuth
				}
				return err
			}
			if err := os.Rename(tmp, out); err != nil {
				return err
			}
			c.Success("Decrypted %s (%s) to %s", in, bmgrHuman(n), out)
			return nil
		},
	})
}
