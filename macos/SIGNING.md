# Code signing and notarization

`macos/build.sh` signs and notarizes the macOS app when it has a Developer ID
identity and notary credentials, and the release workflow
([`.github/workflows/release.yml`](../.github/workflows/release.yml)) gives it
both from the repo's Actions secrets. Without a `DEVELOPER_ID`, the script
falls back to an ad-hoc signature, and Gatekeeper stops users from opening a
downloaded copy until they override it.

## What a signed build does

With `DEVELOPER_ID` set to a Developer ID Application identity in the
keychain, `RELEASE=1 ./macos/build.sh`:

1. signs the Go helper (`Contents/Resources/openmessage`) with the hardened
   runtime and [`OpenMessageHelper.entitlements`](OpenMessageHelper.entitlements),
   which grants no exceptions;
2. signs `OpenMessage.app` with the hardened runtime and
   [`OpenMessage.entitlements`](OpenMessage.entitlements), which leaves the
   app unsandboxed and allows two exceptions:
   `com.apple.security.cs.disable-library-validation` and
   `com.apple.security.cs.allow-dyld-environment-variables`;
3. builds `macos/build/OpenMessage.dmg` and signs the DMG itself, because
   Gatekeeper checks the disk image's own signature before it mounts it;
4. submits the DMG to Apple's notary service, waits for the verdict, and
   staples the ticket to the DMG.

For step 4 the script uses `AC_USERNAME`, `AC_PASSWORD` and `AC_TEAM_ID` when
it has all three. Otherwise it looks for the `notarytool` keychain profile
that `NOTARY_KEYCHAIN_PROFILE` names (default `OpenMessage`). With
neither, it signs the DMG, skips notarization and prints "Signed but NOT
notarized" without failing.

Leave out `RELEASE=1` and the script builds a dev bundle
(`com.openmessage.app.dev`, in `OpenMessage-dev.dmg`) that can't
shadow the installed app; see "Building the macOS app" in
[`CLAUDE.md`](../CLAUDE.md). Anything you install or ship needs `RELEASE=1`.

## How the release workflow signs

Pushing a `v*` tag runs the release workflow. Its macOS job:

1. imports the certificate from `MACOS_CERT_P12_BASE64` into a temporary
   keychain under `$RUNNER_TEMP`, then deletes the decoded `.p12` (the step
   skips when the repo has no such secret);
2. runs `macos/build.sh` with `RELEASE=1`, `DEVELOPER_ID` and the three `AC_*`
   secrets;
3. fails the job if the bundle carries any id other than
   `com.openmessage.app` or any name other than `OpenMessage`, or if
   `OpenMessage.dmg` doesn't exist;
4. runs `spctl --assess --type install` on the DMG and, if Gatekeeper rejects
   it, prints a warning without failing the job.

Because of step 4, and because `build.sh` skips notarization without failing
when a notary secret goes missing, a green run doesn't prove the DMG passes
Gatekeeper. Check the published DMG yourself (see "Verifying"). The publish job
then attaches the DMG, the four CLI tarballs and `SHA256SUMS` to the GitHub
release. Work through [the release checklist](../docs/release-checklist.md)
before you push the tag.

## Secrets

The workflow reads six repository secrets:

| Secret | Value |
|---|---|
| `DEVELOPER_ID` | The full identity name from `security find-identity -v -p codesigning` (e.g. `Developer ID Application: Your Name (TEAMID)`) |
| `MACOS_CERT_P12_BASE64` | The base64 text of a `.p12` file that holds the Developer ID Application certificate and its private key |
| `MACOS_CERT_PASSWORD` | The password you set when you exported the `.p12` |
| `AC_USERNAME` | The Apple ID email of the developer account |
| `AC_PASSWORD` | An app-specific password for that Apple ID |
| `AC_TEAM_ID` | The 10-character Apple Developer Team ID (developer.apple.com/account) |

Update the matching secret whenever you replace the certificate or the
app-specific password.

## Setting up credentials

You need an Apple Developer Program membership: Developer ID certificates and
notarization both require one.

### 1. Developer ID Application certificate

In Xcode → Settings → Accounts → Manage Certificates → `+` → "Developer ID
Application". Xcode installs the certificate in your login keychain. Find its
identity name with:

```bash
security find-identity -v -p codesigning
```

### 2. App-specific password

Apple ID → [appleid.apple.com](https://appleid.apple.com) → Sign-In and
Security → App-Specific Passwords → Generate. Label it "OpenMessage notary".

### 3. Repository secrets

In Keychain Access, right-click the certificate → Export → `.p12`, and set a
password. Then store the six secrets with `gh`, which prompts for any value
you don't pipe in:

```bash
base64 -i DeveloperID.p12 | gh secret set MACOS_CERT_P12_BASE64 --repo MaxGhenis/openmessage
gh secret set MACOS_CERT_PASSWORD --repo MaxGhenis/openmessage
gh secret set DEVELOPER_ID --repo MaxGhenis/openmessage
gh secret set AC_USERNAME --repo MaxGhenis/openmessage
gh secret set AC_PASSWORD --repo MaxGhenis/openmessage
gh secret set AC_TEAM_ID --repo MaxGhenis/openmessage
```

Delete the exported `.p12` once the secret holds it.

## Local signed builds

To sign and notarize on your own Mac, store notary credentials in your
keychain once; `notarytool` prompts for the app-specific password:

```bash
xcrun notarytool store-credentials OpenMessage --apple-id you@example.com --team-id TEAMID
```

Then build with your identity:

```bash
DEVELOPER_ID="Developer ID Application: Your Name (TEAMID)" RELEASE=1 ./macos/build.sh
```

`build.sh` finds the `OpenMessage` profile by itself; set
`NOTARY_KEYCHAIN_PROFILE` if you saved it under another name.

## Verifying

A notarized, stapled DMG passes both checks:

```bash
spctl --assess --type install --verbose macos/build/OpenMessage.dmg
# Expected: macos/build/OpenMessage.dmg: accepted
#           source=Notarized Developer ID
xcrun stapler validate macos/build/OpenMessage.dmg
```

To check a published release, download its DMG and point the same commands at
the downloaded file:

```bash
gh release download <tag> --repo MaxGhenis/openmessage --pattern OpenMessage.dmg
```
