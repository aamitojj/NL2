Binary assets stored as base64 text (the repo is synced via a text-only tool).
CI decodes them before building:
  assets/web_icon.png.b64      -> web/icon.png
  assets/winres_icon.ico.b64   -> winres/icon.ico
  assets/winres_icon.png.b64   -> winres/icon.png
rsrc_windows_amd64.syso is generated in CI with go-winres from winres/winres.json.
