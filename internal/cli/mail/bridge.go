package mail

import (
	"github.com/roman-16/proton-cli/internal/cli/kit"
	"github.com/roman-16/proton-cli/internal/imapbridge"
	"github.com/roman-16/proton-cli/internal/imapserver"
	"github.com/roman-16/proton-cli/internal/smtpbridge"
	"github.com/spf13/cobra"
)

func bridgeCmd() *cobra.Command {
	c := &cobra.Command{Use: "bridge", Short: "Connect a desktop mail client"}
	c.AddCommand(bridgeServeCmd())
	return c
}

func bridgeServeCmd() *cobra.Command {
	var addr, smtpAddr string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Serve mail to a local desktop client",
		Long: "Serve mail over local IMAP and SMTP connections on loopback only. Thunderbird can move messages between folders; other IMAP mailbox changes are read-only.\n\n" +
			"Folder names load when listed, and messages load when fetched or searched. SMTP sends through your selected profile without client authentication; IMAP accepts any username and password for that profile.",
		RunE: kit.Run(nil, func(c *kit.Invocation) error {
			if err := c.App.Authenticate(c.Ctx); err != nil {
				return err
			}
			if addr == "" {
				addr = "127.0.0.1:1143"
			}
			backend, err := imapbridge.New(c.Ctx, c.App.Mail)
			if err != nil {
				return kit.Fail("Could not start the local IMAP server: %s", err).Exit(2)
			}
			server := imapserver.New(backend)
			defer server.Close()
			listener, err := server.Listen(addr)
			if err != nil {
				return kit.Fail("Could not start the local IMAP server: %s", err).Exit(2)
			}
			smtp := smtpbridge.New(c.Ctx, c.App.Mail)
			defer smtp.Close()
			smtpListener, err := smtp.Listen(smtpAddr)
			if err != nil {
				return kit.Fail("Could not start the local SMTP server: %s", err).Exit(2)
			}
			stopped := make(chan struct{})
			defer close(stopped)
			go func() {
				select {
				case <-c.Ctx.Done():
					_ = server.Close()
					_ = smtp.Close()
				case <-stopped:
				}
			}()
			c.UI().Hint("IMAP listening at " + listener.Addr().String() + "; SMTP listening at " + smtpListener.Addr().String() + ". Press Ctrl+C to stop.")
			serveErr := make(chan error, 2)
			go func() { serveErr <- server.Serve(listener) }()
			go func() { serveErr <- smtp.Serve(smtpListener) }()
			err = <-serveErr
			_ = server.Close()
			_ = smtp.Close()
			if c.Ctx.Err() != nil {
				return nil
			}
			return err
		}),
	}
	c.Flags().StringVar(&addr, "listen", "127.0.0.1:1143", "Loopback address to listen on")
	c.Flags().StringVar(&smtpAddr, "smtp-listen", "127.0.0.1:1025", "Loopback address for SMTP submissions")
	return c
}
