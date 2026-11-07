import AppKit
import WebKit

/// Insta360's own sign-in page (reCAPTCHA-gated, so it can't be scripted) in a web view. The user
/// signs in; we watch the page's storage, cookies and any messages it posts to its host app for
/// the session JWT. Uses a throwaway data store, so nothing lingers in WebKit afterwards.
@MainActor
public final class CloudSignIn: NSObject, WKScriptMessageHandler, WKNavigationDelegate, NSWindowDelegate {
    public let window: NSWindow
    private let web: WKWebView
    private var timer: Timer?
    private var completion: ((CloudSession?) -> Void)?
    private var finished = false

    /// Studio hosts this page in CEF; it may hand the token to the host through a bridge.
    /// Forward any such calls to us (they're scanned for a JWT, never stored otherwise).
    private static let bridgeJS = """
    (function () {
      function post(x) { try { window.webkit.messageHandlers.reframe.postMessage(typeof x === 'string' ? x : JSON.stringify(x)); } catch (e) {} }
      window.cefQuery = function (req) { post(req && req.request !== undefined ? req.request : req); if (req && req.onSuccess) req.onSuccess(''); return 1; };
      window.cefQueryCancel = function () {};
      window.addEventListener('message', function (e) { post(e.data); });
    })();
    """

    private static let storageJS = """
    (function () {
      var out = [];
      try { for (var i = 0; i < localStorage.length; i++) out.push(localStorage.getItem(localStorage.key(i))); } catch (e) {}
      try { for (var i = 0; i < sessionStorage.length; i++) out.push(sessionStorage.getItem(sessionStorage.key(i))); } catch (e) {}
      try { out.push(document.cookie); } catch (e) {}
      return out.join('\\n');
    })()
    """

    public override init() {
        let cfg = WKWebViewConfiguration()
        cfg.websiteDataStore = .nonPersistent()
        let ucc = WKUserContentController()
        ucc.addUserScript(WKUserScript(source: Self.bridgeJS, injectionTime: .atDocumentStart, forMainFrameOnly: false))
        cfg.userContentController = ucc
        web = WKWebView(frame: NSRect(x: 0, y: 0, width: 520, height: 720), configuration: cfg)
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 520, height: 720),
                          styleMask: [.titled, .closable, .resizable], backing: .buffered, defer: false)
        super.init()
        ucc.add(self, name: "reframe")
        web.navigationDelegate = self
        window.title = "Sign in to Insta360 Cloud"
        window.contentView = web
        window.isReleasedWhenClosed = false
        window.delegate = self
        window.center()
    }

    /// Show the window and call back once with the captured session, or nil if the window was
    /// closed or `timeout` passed.
    public func begin(timeout: TimeInterval = 600, completion: @escaping (CloudSession?) -> Void) {
        self.completion = completion
        web.load(URLRequest(url: CloudClient.loginURL))
        // Launched from a terminal or agent the app can't take focus (macOS activation is
        // cooperative), so float the window above everything and front it regardless.
        window.level = .floating
        window.makeKeyAndOrderFront(nil)
        window.orderFrontRegardless()
        NSApp.activate()
        let start = Date()
        timer = Timer.scheduledTimer(withTimeInterval: 1.5, repeats: true) { [weak self] _ in
            MainActor.assumeIsolated {
                guard let self else { return }
                if Date().timeIntervalSince(start) > timeout { self.finish(nil); return }
                self.poll()
            }
        }
    }

    private func poll() {
        web.evaluateJavaScript(Self.storageJS) { [weak self] result, _ in
            MainActor.assumeIsolated {
                if let s = result as? String { self?.consider(s) }
            }
        }
        web.configuration.websiteDataStore.httpCookieStore.getAllCookies { [weak self] cookies in
            MainActor.assumeIsolated {
                self?.consider(cookies.map(\.value).joined(separator: "\n"))
            }
        }
    }

    private func consider(_ text: String) {
        guard !finished, let token = CloudSession.findToken(in: text) else { return }
        finish(CloudSession(token: token))
    }

    private func finish(_ s: CloudSession?) {
        guard !finished else { return }
        finished = true
        timer?.invalidate()
        window.orderOut(nil)
        completion?(s)
        completion = nil
    }

    public nonisolated func userContentController(_ ucc: WKUserContentController, didReceive message: WKScriptMessage) {
        let body = "\(message.body)"
        MainActor.assumeIsolated { consider(body) }
    }

    public nonisolated func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        MainActor.assumeIsolated { poll() }
    }

    public nonisolated func windowWillClose(_ notification: Notification) {
        MainActor.assumeIsolated { finish(nil) }
    }
}
