# proton mail bridge

Connect a desktop mail client.

Every command under `proton mail bridge`, with the arguments and flags it takes. For these commands in use, see [the mail guide](README.md).

Holds `serve`.

## `serve`

Serve mail over local IMAP and SMTP connections on loopback only. Thunderbird can move messages between folders; other IMAP mailbox changes are read-only.

Folder names load when listed, and messages load when fetched or searched. SMTP sends through your selected profile without client authentication; IMAP accepts any username and password for that profile.

```
proton mail bridge serve
```

```bash
proton mail bridge serve
proton mail bridge serve --listen 127.0.0.1:1144 --smtp-listen 127.0.0.1:1026
```

| Flag | Description |
| --- | --- |
| `--listen string` | Loopback address to listen on (default `127.0.0.1:1143`) |
| `--smtp-listen string` | Loopback address for SMTP submissions (default `127.0.0.1:1025`) |

---

Every command also takes the [flags that work everywhere](../about/commands.md#flags-that-work-on-every-command).
