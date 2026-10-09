# Multi-client terminal rendering (Agent v0.1.16)

A shared PTY has one authoritative grid and one controller. Read-only viewers
preserve that grid and scroll horizontally/vertically, rather than wrapping cursor
commands at their own screen width. The terminal header exposes **只读 · 接管**;
taking control changes both browsers' roles and fits the PTY to the new controller.
Controller disconnect promotes the remaining earliest viewer and notifies its UI.

Previously each Agent view sent the same session-ID frame, which the Server then
broadcast to every subscriber. With N views, live bytes arrived N times at each
browser; new-connection replay also reset existing browsers. Agent and Server now
negotiate `terminal_views` in hello/ready. Each approved attach gets a Server-generated
opaque ID, mapped to its authenticated client, device and session. The Agent link
uses a derived UUID as the binary stream ID; the Server delivers only to that view
and rewrites the stream ID to the session UUID for unchanged browser protocols.
Input uses the same authenticated view identity, so both Server and Agent enforce
controller status even during a takeover race. No JSON/base64 wrapper or extra
binary-header bytes are added; compression policy remains unchanged.

State messages provide role and actual PTY grid before replay and resize output.
xterm drains earlier writes before changing the parsing grid. The Session records
actual applied dimensions, serializes resize calls, and cancels the previous
controller's pending resize on takeover/disconnect. Reattaching the same browser
reuses its native view and preserves role. A failed replay cannot reserve a phantom
controller. State enqueue is nonblocking under the fanout lock, retries before
bytes, and respects output-drop recovery.

View routes are removed on detach, disconnect and session deletion. Queued output
for an already removed view is discarded without broadcasting or disconnecting the
Agent; known targets belonging to another device/user remain forbidden.
Older Agents retain legacy transport and report an explicit update requirement
when asked to take control. New Agents fall back on older Servers through the
ready capability flag. Upgrade Server and Agent together to obtain the rendering
fix; existing shared terminal processes need the new Agent runtime.
