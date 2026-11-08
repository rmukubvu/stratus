# Releasing the SDKs

The TypeScript SDK is published to npm and the Python SDK to PyPI, both as
`stratus-emulator`, by the [SDK release workflow](../.github/workflows/sdk-release.yml).
It runs when a tag like `sdk-v0.1.0` is pushed:

1. **verify** checks the tag matches both package versions, starts Stratus,
   runs both test suites, and builds both packages.
2. **npm** and **pypi** publish, each in its own GitHub environment.

No long-lived tokens are stored. Both registries trust this repository's
workflow directly through OpenID Connect ("trusted publishing"), and npm
packages get a provenance statement that links each version to the commit
and run that built it.

Running the workflow by hand (Actions → SDK release → Run workflow) does
everything except publish. Use it as a dry run.

## One-time setup

### 1. Accounts

- **npm**: sign up at <https://www.npmjs.com/signup> and turn on two-factor
  authentication (Account → Two-Factor Authentication).
- **PyPI**: sign up at <https://pypi.org/account/register/>. PyPI requires
  two-factor authentication before you can publish.

### 2. PyPI: add a pending trusted publisher

This reserves the name for this repository before the first upload.

PyPI → Your account → Publishing → *Add a new pending publisher* → GitHub:

| Field | Value |
| --- | --- |
| PyPI project name | `stratus-emulator` |
| Owner | `rmukubvu` |
| Repository name | `stratus` |
| Workflow name | `sdk-release.yml` |
| Environment name | `pypi` |

### 3. npm: first publish with a short-lived token

npm only lets you configure trusted publishing on a package that already
exists, so the first version goes out with a token:

1. npm → Access Tokens → *Generate New Token* → *Granular Access Token*.
   Give it **Read and write** permission on packages, an expiry of 7 days,
   and no organisation access.
2. GitHub → this repository → Settings → Secrets and variables → Actions →
   *New repository secret*: name `NPM_TOKEN`, value the token.

### 4. GitHub environments

GitHub → Settings → Environments → *New environment*: create `npm` and
`pypi`. Adding yourself as a required reviewer on both is optional but
recommended, so every publish waits for your click.

## Releasing a version

1. Set the same version in `sdk/typescript/package.json`,
   `sdk/python/pyproject.toml` and `sdk/python/src/stratus_emulator/__init__.py`,
   then merge to main. The verify job fails if they differ.
2. Tag the merge commit and push the tag:
   ```sh
   git tag sdk-v0.1.0
   git push origin sdk-v0.1.0
   ```
3. Approve the `npm` and `pypi` deployments if you added reviewers.

## After the first release: switch npm to trusted publishing

1. npm → the `stratus-emulator` package → Settings → *Trusted Publisher* →
   GitHub Actions: organisation or user `rmukubvu`, repository `stratus`,
   workflow filename `sdk-release.yml`, environment `npm`.
2. On the same page, under *Publishing access*, choose to require two-factor
   authentication and disallow tokens.
3. Delete the `NPM_TOKEN` secret from GitHub, and revoke the token on npm.

From then on, releases need only a tag.
