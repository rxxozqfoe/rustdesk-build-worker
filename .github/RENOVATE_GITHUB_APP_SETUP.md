# Setting up Renovate with a GitHub App

This document describes how to set up a GitHub App so that
`.github/workflows/renovate.yml` can authenticate Renovate with an App
installation token. App creation is a one-time GitHub UI flow — nothing
here is automated.

---

## 1. Create the GitHub App

1. Open <https://github.com/settings/apps/new> (personal scope) or
   `https://github.com/organizations/<ORG>/settings/apps/new` (org
   scope). Use the org scope if this repo is org-owned and you want
   multiple repos to share one App.
2. **GitHub App name**: pick something specific, e.g.
   `rxxozqfoe-renovate`. App names are globally unique on GitHub.
3. **Homepage URL**: any URL is fine
   (e.g. <https://github.com/rxxozqfoe/rustdesk-build-worker>).
4. **Webhook**: untick **Active**. Renovate self-hosted does not need
   webhooks because the workflow polls on a cron.
5. **Repository permissions** — set these to *Read and write* unless
   noted otherwise:

   | Permission       | Access         | Why                                                                      |
   | ---------------- | -------------- | ------------------------------------------------------------------------ |
   | Contents         | Read and write | Create branches, push commits.                                           |
   | Pull requests    | Read and write | Open / update / close Renovate PRs.                                      |
   | Workflows        | Read and write | Update `.github/workflows/*.yml` (GITHUB_TOKEN cannot do this).          |
   | Issues           | Read and write | Maintain the Dependency Dashboard issue.                                 |
   | Commit statuses  | Read and write | Set `renovate/stability-days` status used by `minimumReleaseAge`.        |
   | Metadata         | Read           | Required for everything else; GitHub forces this on.                     |

   Leave all other repository permissions at *No access*.
6. **Organization permissions**: leave at *No access* unless you also
   want Renovate to manage org-level secrets or members (you don't).
7. **Account permissions**: leave at *No access*.
8. **Where can this GitHub App be installed?**
   - *Only on this account* if you keep the App private.
   - *Any account* only if you intend to share it. Not recommended.
9. Click **Create GitHub App**.

---

## 2. Generate a private key

On the App page (Settings → Developer settings → GitHub Apps → your App):

1. Scroll to **Private keys** → **Generate a private key**.
2. A `.pem` file is downloaded immediately. Keep it; you cannot
   re-download it from the UI.

---

## 3. Install the App on this repository

1. On the App page, click **Install App** in the left sidebar.
2. Pick the account that owns `rxxozqfoe/rustdesk-build-worker`.
3. Choose **Only select repositories** and tick
   `rxxozqfoe/rustdesk-build-worker`.
4. Confirm install.
5. After install, the URL bar shows
   `https://github.com/settings/installations/<INSTALLATION_ID>`
   (or `https://github.com/organizations/<ORG>/settings/installations/<INSTALLATION_ID>`).
   Copy the numeric `INSTALLATION_ID`.

---

## 4. Record the three values

You now have three things you need:

| Value             | Where it comes from                                  |
| ----------------- | ---------------------------------------------------- |
| `APP_ID`          | App page, "About" section, numeric **App ID** field. |
| `INSTALLATION_ID` | The numeric ID from the install URL in step 3.5.     |
| `APP_PRIVATE_KEY` | The contents of the `.pem` file from step 2.         |

---

## 5. Store the values as repository secrets

Repository → Settings → Secrets and variables → Actions → **New
repository secret**, for each:

- `RENOVATE_APP_ID` ← `APP_ID` (numeric)
- `RENOVATE_APP_INSTALLATION_ID` ← `INSTALLATION_ID` (numeric)
- `RENOVATE_APP_PRIVATE_KEY` ← full `.pem` contents including the
  `-----BEGIN ...-----` / `-----END ...-----` lines

---

## 6. Point `.github/workflows/renovate.yml` at the App

The **Run Renovate** step takes a short-lived installation token minted
by the official
[`actions/create-github-app-token`](https://github.com/actions/create-github-app-token)
action from the App ID and private-key secrets above (the action
resolves the installation itself from `owner`/`repositories`, so
`RENOVATE_APP_INSTALLATION_ID` is not consumed here):

```yaml
      - name: Mint Renovate installation token
        id: app-token
        uses: actions/create-github-app-token@bcd2ba49218906704ab6c1aa796996da409d3eb1 # v3.2.0
        with:
          app-id: ${{ secrets.RENOVATE_APP_ID }}
          private-key: ${{ secrets.RENOVATE_APP_PRIVATE_KEY }}
          # Scope the token to this repo only. Without this, the token
          # would be scoped to every repo the App is installed in.
          owner: ${{ github.repository_owner }}
          repositories: ${{ github.event.repository.name }}
          # Pin the token's scopes in code. Listing any permission-*
          # makes the token contain ONLY these (+ forced metadata:read),
          # so it can't inherit extra access if the App is later widened.
          permission-contents: write       # create branches, push commits
          permission-pull-requests: write  # open / update / close PRs
          permission-issues: write         # Dependency Dashboard issue
          permission-statuses: write       # renovate/stability-days status
          permission-workflows: write      # update .github/workflows/*

      - name: Run Renovate
        uses: renovatebot/github-action@3633cede7d4d4598438e654eac4a695e46004420 # v46.1.7
        with:
          configurationFile: renovate.json
          token: ${{ steps.app-token.outputs.token }}
        env:
          LOG_LEVEL: ${{ inputs.logLevel || 'info' }}
          RENOVATE_DRY_RUN: ${{ inputs.dryRun && 'full' || '' }}
          RENOVATE_REPOSITORIES: ${{ github.repository }}
```

Pin `actions/create-github-app-token` to a commit SHA the same way the
rest of the workflow does (Renovate will manage future bumps via
`helpers:pinGitHubActionDigestsToSemver`).

The token Renovate receives is automatically scoped to the repositories
you listed, expires after one hour, and is revoked at job end.

---

## 7. Verify

1. Trigger Renovate manually:
   *Actions → Renovate → Run workflow* (with `dryRun: true` the first
   time).
2. Watch the run. The "Mint Renovate installation token" step should
   succeed; the "Run Renovate" step should pick up the expected set of
   updates.
3. Re-run with `dryRun: false` and confirm it opens a PR.

---

## Optional: install on more repos later

If you later want this App to manage another repo, install it from the
App page (step 3) onto that repo, and update the `repositories:` list on
the token step (or drop it to let the token cover every installed repo).
