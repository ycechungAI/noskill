// S7 notification helper: posts one notification with Approve / Skip buttons through
// UNUserNotificationCenter and forwards the click to the daemon's Unix socket. Throwaway.
//   noskill-notify --id X --title T --body B [--socket PATH] [--timeout SECS]
// With no --id (macOS relaunched us because the user clicked a notification after we exited) it only
// listens for the response for 20 s.
import Cocoa
import UserNotifications

setbuf(stdout, nil)
var id = "", title = "noskill", body = "", timeout = 120.0
var socketPath = NSHomeDirectory() + "/.noskill-s7/run/s7.sock"
var a = Array(CommandLine.arguments.dropFirst())
while !a.isEmpty {
    let k = a.removeFirst()
    guard !a.isEmpty else { break }
    let v = a.removeFirst()
    switch k {
    case "--id": id = v
    case "--title": title = v
    case "--body": body = v
    case "--socket": socketPath = v
    case "--timeout": timeout = Double(v) ?? 120
    default: break
    }
}

func log(_ s: String) { FileHandle.standardError.write(Data(("\(Date()) \(s)\n").utf8)) }

func sendToDaemon(_ obj: [String: Any]) {
    log("send \(obj)")
    guard let data = try? JSONSerialization.data(withJSONObject: obj) else { return }
    let fd = socket(AF_UNIX, SOCK_STREAM, 0)
    if fd < 0 { return }
    defer { close(fd) }
    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    let cap = MemoryLayout.size(ofValue: addr.sun_path)
    withUnsafeMutablePointer(to: &addr.sun_path) { p in
        p.withMemoryRebound(to: CChar.self, capacity: cap) { _ = strncpy($0, socketPath, cap - 1) }
    }
    let rc = withUnsafePointer(to: &addr) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
    }
    if rc != 0 { log("connect failed errno=\(errno)"); return }
    let line = Array(data) + [10]
    _ = write(fd, line, line.count)
    var buf = [UInt8](repeating: 0, count: 8)
    _ = read(fd, &buf, 8) // wait for the daemon's "ok"
}

final class App: NSObject, NSApplicationDelegate, UNUserNotificationCenterDelegate {
    func applicationDidFinishLaunching(_ n: Notification) {
        let c = UNUserNotificationCenter.current()
        c.delegate = self
        log("launched id=\(id) bundle=\(Bundle.main.bundleIdentifier ?? "nil") pid=\(getpid())")
        if id.isEmpty {
            DispatchQueue.main.asyncAfter(deadline: .now() + 20) { exit(0) }
            return
        }
        let approve = UNNotificationAction(identifier: "approve", title: "Approve", options: [])
        let skip = UNNotificationAction(identifier: "skip", title: "Skip", options: [])
        c.setNotificationCategories([UNNotificationCategory(identifier: "noskill.approval", actions: [approve, skip], intentIdentifiers: [], options: [.customDismissAction])])
        c.requestAuthorization(options: [.alert, .sound]) { granted, err in
            sendToDaemon(["type": "auth", "id": id, "granted": granted, "err": err.map { "\($0)" } ?? ""])
            guard granted else { exit(3) }
            c.getNotificationSettings { s in
                sendToDaemon(["type": "settings", "id": id, "alertStyle": s.alertStyle.rawValue, "authStatus": s.authorizationStatus.rawValue,
                              "notificationCenter": s.notificationCenterSetting.rawValue, "lockScreen": s.lockScreenSetting.rawValue])
            }
            let content = UNMutableNotificationContent()
            content.title = title
            content.body = body
            content.sound = .default
            content.categoryIdentifier = "noskill.approval"
            content.userInfo = ["id": id]
            c.add(UNNotificationRequest(identifier: id, content: content, trigger: nil)) { err in
                sendToDaemon(["type": "posted", "id": id, "err": err.map { "\($0)" } ?? ""])
                if err != nil { exit(4) }
            }
        }
        DispatchQueue.main.asyncAfter(deadline: .now() + timeout) {
            sendToDaemon(["type": "timeout", "id": id])
            exit(0)
        }
    }

    func userNotificationCenter(_ c: UNUserNotificationCenter, willPresent n: UNNotification, withCompletionHandler h: @escaping (UNNotificationPresentationOptions) -> Void) {
        h([.banner, .list, .sound])
    }

    func userNotificationCenter(_ c: UNUserNotificationCenter, didReceive r: UNNotificationResponse, withCompletionHandler h: @escaping () -> Void) {
        let nid = (r.notification.request.content.userInfo["id"] as? String) ?? r.notification.request.identifier
        sendToDaemon(["type": "response", "id": nid, "action": r.actionIdentifier, "relaunched": id.isEmpty])
        h()
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.5) { exit(0) }
    }
}

let app = NSApplication.shared
app.setActivationPolicy(.accessory)
let delegate = App()
app.delegate = delegate
app.run()
