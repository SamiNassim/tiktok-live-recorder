# Publishing a release

Keep committing directly to `master`. Pull requests are not required.

## First-time setup

- Put the repository on GitHub and configure it as the `origin` remote.
- Review and commit the README and any unfinished changes.

## Create a release

Run these commands from the project folder:

```sh
go run ./tools/release prepare v0.1.0
git push --atomic origin master v0.1.0
```

Use `v0.1.0` for the first release, then increase the version, for example
`v0.1.1`. The first command updates the app version and creates a local tag.
The push starts GitHub's tests and builds for Mac, Windows, and Linux.

Downloads are a DMG for Mac, a portable `.exe` for Windows, and Linux packages.
Windows releases do not include an installer or ZIP.

## Review and publish

1. On GitHub, open the repository's **Actions** tab and wait for **Release** to succeed.
2. Open **Releases** and select the draft.
3. Review the notes, test the downloads, then click **Publish release**.

The notes include commit messages and descriptions since the previous release.
You can edit them in the draft. Builds use no Apple or Windows publisher
certificates; first-launch instructions are in the [README](README.md#download-and-open).
