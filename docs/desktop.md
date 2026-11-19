# Desktop

The desktop is a web page served by `dc-agent` at `/desktop/`. It draws the dock, every window frame, and the browser toolbar in CSS and JavaScript. It has no build step and no GTK code of its own.

## Windows

Each window is a rounded frame with a 40px titlebar and the familiar red, yellow and green buttons. The window content is one of two things:

- **GTK apps** (Terminal, Files, Text Editor): an iframe onto the app's own Broadway display. The agent starts the display and the app when the window opens. The app runs with client-side decorations off (`GTK_CSD=0`), so it fills the iframe exactly.
- **Browser**: a canvas that shows headless Chromium's screencast, with a toolbar for Back, Forward, Reload, and an address bar.

Windows can be:

- dragged by the titlebar, and snap to the left or right half when released near a side edge;
- resized from any edge or corner (minimum 280 × 160);
- minimized (red/yellow button), maximized (green button or double-click the titlebar), and closed (the close button closes the window and keeps the app running);
- focused by clicking, which brings them to the front.

Every window stays fully on screen, so nothing can be dragged out of reach.

## Dock

The dock sits at the bottom, centered, above every window. A dot under an icon marks an app with an open window. Clicking an icon:

- opens the app if it has no window;
- restores it if it is minimized;
- minimizes it if it is already the focused window;
- otherwise brings it to the front.

## Browser address bar

The address bar shows the page's real URL and title, read from Chromium. It is not a static input. While you type in it the address is not overwritten. After you press Enter the typed text stays until the page moves on, and Escape restores the real URL.

Input is forwarded to the page: mouse, scroll, keys, and Ctrl+V paste. F11, F12 and Ctrl+Shift+I/C/J stay with the desktop.

## Keyboard

Printable keys reach GTK apps directly. The `dc-windows` module stops GTK from treating plain letters as menubar mnemonics, so typing in an app is not swallowed. Browser-only shortcuts (F11, F12, Ctrl+Shift+I/C/J) are kept out of the forwarded input, so the browser's own dev tools still work.

## Limits

- Window placement is saved in the browser's local storage per desktop, not per user.
- Closing a window does not quit its app. Quitting an app from the desktop is not implemented.
- The browser screencast is JPEG at about 12 frames per second. It is good for browsing, not for video.
- GTK apps under Broadway do not get native clipboard integration beyond what the iframe allows.

## Development

`DC_DESKTOP_DIR` serves the desktop from a directory instead of the embedded copy, so CSS and JavaScript changes can be tried without rebuilding the image:

```bash
docker run --rm -p 8080:8080 -e COMPUTER_AGENT_TOKEN=dev-token \
  -e DC_DESKTOP_DIR=/desk -v "$PWD/cmd/dc-agent/desktop:/desk:ro" docker-computer:dev
```
