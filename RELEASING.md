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

## If no draft appears

Open **Actions** first. A draft appears only after all builds succeed.

If the tag is on GitHub but no run started, open **Actions → Release → Run workflow**.
Leave the workflow branch set to **master**, enter the existing tag (for example,
`v0.1.0`), and click **Run workflow**. This builds the tagged code without moving
the tag. If a run failed, open it and choose **Re-run failed jobs** instead.

## If you change your mind

If only the notes need changing, edit the draft directly.

For a code change or an abandoned release:

1. If the **Release** workflow is still running, cancel it under **Actions** and wait for it to stop.
2. Delete the draft under **Releases**.
3. If needed, fix the code and commit normally. To undo a commit, use `git revert <commit>`.
4. When ready, run the release commands again with a new version, such as `v0.1.1`.

Keep the old tag unchanged and do not rerun its workflow. Deleting a draft does
not undo pushed commits: in a public repository, the pushed source is already
visible. The draft controls publication of the release downloads.
