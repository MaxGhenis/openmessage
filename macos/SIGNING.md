# Code signing and notarization

`macos/build.sh` signs and notarizes the macOS app when it has a Developer ID
identity and notary credentials, and the release workflow
([`.github/workflows/release.yml`](../.github/workflows/release.yml)) gives it
both from the repo's Actions secrets. Without a `DEVELOPER_ID`, the script
falls back to an ad-hoc signature, and Gatekeeper stops users from opening a
downloaded copy until they override it.

## What a signed build does

If `DEVELOPER_ID` names a Developer ID Application identity in the keychain,
`RELEASE=1 ./macos/build.sh`:

1. signs the Go helper (`Contents/Resources/openmessage`) with the hardened
   runtime and [`OpenMessageHelper.entitlements`](OpenMessageHelper.entitlements),
   which grants no exceptions;
2. signs `OpenMessage.app` with the hardened runtime and
   [`OpenMessage.entitlements`](OpenMessage.entitlements), which leaves the
   app unsandboxed and allows two exceptions:
   `com.apple.security.cs.disable-library-validation` and
   `com.apple.security.cs.allow-dyld-environment-variables`;
3. builds `macos/build/OpenMessage.dmg` and signs the DMG itself, because
   Gatekeeper checks a disk image's signature before mounting it;
4. submits the DMG to Apple's notary service, waits for the verdict, and
   staples the ticket to the DMG.

For step 4 the script uses `AC_USERNAME`, `AC_PASSWORD` and `AC_TEAM_ID` when
it has all three. Otherwise it tries the `notarytool` keychain profile that
`NOTARY_KEYCHAIN_PROFILE` names (default `OpenMessage`), and first checks it
with `notarytool history`, which asks Apple's notary service. If that check
fails for any reason, such as a missing profile, a revoked app-specific
password or no network, the script skips notarization and prints "Signed but
NOT notarized" without failing; the DMG keeps its step 3 signature.

Leave out `RELEASE=1` and the script builds a dev bundle
(`com.openmessage.app.dev`, in `OpenMessage-dev.dmg`) that can't
shadow the installed app; see "Building the macOS app" in
[`CLAUDE.md`](../CLAUDE.md). Anything you install or ship needs `RELEASE=1`.

## How the release workflow signs

Work through [the release checklist](../docs/release-checklist.md), then push
a `v*` tag to run the release workflow. Its macOS job:

1. imports the certificate from `MACOS_CERT_P12_BASE64` into a temporary
   keychain under `$RUNNER_TEMP`, then deletes the decoded `.p12` (the
   workflow skips this step when the repo has no `MACOS_CERT_P12_BASE64`
   secret);
2. runs `macos/build.sh` with `RELEASE=1`, `DEVELOPER_ID` and the three `AC_*`
   secrets;
3. fails the job unless the bundle id reads `com.openmessage.app`, the bundle
   name reads `OpenMessage` and `OpenMessage.dmg` exists;
4. runs `spctl --assess --type install` on the DMG and, if Gatekeeper rejects
   it, prints a warning without failing the job (like step 1, the workflow
   skips this step without `MACOS_CERT_P12_BASE64`).

The publish job then attaches the DMG, the four CLI tarballs and `SHA256SUMS`
to the GitHub release.

A green run can publish a DMG that Gatekeeper rejects: step 4 warns instead of
failing, and `build.sh` skips notarization without failing when the job lacks
an `AC_*` secret. With neither `MACOS_CERT_P12_BASE64` nor `DEVELOPER_ID`, the
job ships an ad-hoc-signed DMG and skips the Gatekeeper check. Check the
published DMG yourself (see "Verifying"). If it fails, fix the secret, then
either cut a new tag or re-run the original run within 30 days
(`gh run rerun <run-id> --repo MaxGhenis/openmessage`). A re-run builds the
same commit and tag, and its publish job uploads every file again with
`--clobber`, regenerates `SHA256SUMS` and rewrites the release notes. Check
the DMG again afterwards. Don't replace the DMG by hand, because
`SHA256SUMS` would no longer match it.

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

When you replace the certificate, update `MACOS_CERT_P12_BASE64`, plus
`MACOS_CERT_PASSWORD` if you set a new export password and `DEVELOPER_ID` if
the identity name changes. When you replace the app-specific password, update
`AC_PASSWORD` and run `xcrun notarytool store-credentials` again for any local
profile.

## Setting up credentials

You need an [Apple Developer Program](https://developer.apple.com/programs/enroll)
membership: Developer ID certificates and notarization both require one.

### 1. Developer ID Application certificate

In Xcode, open Settings → Accounts → Manage Certificates, click `+` and choose
"Developer ID Application". Xcode installs the certificate in your login
keychain. Find its identity name with:

```bash
security find-identity -v -p codesigning
```

### 2. App-specific password

Sign in at [appleid.apple.com](https://appleid.apple.com), open Sign-In and
Security → App-Specific Passwords and click Generate. Label the password
"OpenMessage notary".

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

Delete the exported `.p12` once you've set `MACOS_CERT_P12_BASE64`.

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

`build.sh` uses the `OpenMessage` profile by default; set
`NOTARY_KEYCHAIN_PROFILE` if you saved it under another name. Check that the
build prints "Notarized and stapled!" and not "Signed but NOT notarized", or
run the commands under "Verifying".

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
