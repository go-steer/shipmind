# Slack apps

Each environment talks to Slack through its own app, so several environments (staging and production) can share one workspace. The app's manifest is switchboard's, vendored at the version shipmind pins, under the names the environment's `shipmind.yaml` gives it:

```yaml
slack:
  app: example-stage              # the app's name and its bot's display name
  command: /example-stage         # its slash command; switchboard ignores the word, but apps in one workspace can't share it
  description: Chat with the staging agents   # optional; replaces switchboard's description
```

## Creating an app

1. From a shipmind checkout at the environment's pin, print its manifest:

   ```sh
   go run ./cmd/shipmind slack-app <env-dir>
   ```

2. https://api.slack.com/apps → **Create New App** → **From an app manifest**, pick the workspace, and paste the output.
3. **Basic Information** → **App-Level Tokens**: generate one with scope `connections:write` (Socket Mode). This is the `xapp-…` token.
4. **Install App**: install it to the workspace. This is the bot token, `xoxb-…`.
5. Store both tokens in Secret Manager, never in a repository. From a shell, paste each one when prompted, so it never lands on disk or in shell history:

   ```sh
   APP=example-stage PROJECT=<project>
   for t in app bot; do
     read -rs -p "$APP Slack $t token: " v; echo
     printf %s "$v" | gcloud secrets create "$APP-slack-$t-token" \
       --project="$PROJECT" --replication-policy=automatic --data-file=-
   done
   ```

   To replace a token later, use `gcloud secrets versions add` with the same name.

Deploying switchboard copies them into the cluster's `switchboard-slack` Secret (keys `app-token` and `bot-token`), which switchboard's Slack overlay reads.

## After a switchboard bump

Moving switchboard's pin re-vendors its manifest, so the pull request shows any scope or event change. Once an environment is on the new pin, rerun `shipmind slack-app` for it and paste the output into the app's **App Manifest** page. Slack shows the changes; if scopes changed, reinstall the app so they reach the bot token.
