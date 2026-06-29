import SwiftUI
import UIKit
import WebKit

// UIKit application lifecycle (instead of the SwiftUI `App`/`WindowGroup`
// scene lifecycle, which is iOS 14+). This hosts the SwiftUI ContentView
// inside a UIHostingController so the app runs on iOS 13 as well. We
// deliberately do NOT adopt the UIScene lifecycle: a single-window app is
// simpler this way, and it keeps `UIApplication.didEnterBackground` /
// `willEnterForeground` (which ServerController observes) firing on iOS 13+.
@UIApplicationMain
final class AppDelegate: UIResponder, UIApplicationDelegate {
    var window: UIWindow?

    // Owned here so it lives for the whole app session. ContentView reaches
    // it via @EnvironmentObject; ServerController itself observes the
    // background/foreground notifications to stop/start the embedded server.
    let server = ServerController()

    func application(
        _ application: UIApplication,
        didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]?
    ) -> Bool {
        // One-time cache flush on app update. The pinned localhost port keeps the
        // WebView's asset URLs stable, so across an app update it could serve a
        // STALE cached bundle (old JS vs. the new index.html) — the mismatch that
        // left a blank screen. Drop the HTTP cache once per version change; normal
        // caching resumes afterward (media stays cached). This touches only the
        // HTTP cache, NOT localStorage (settings/lang/saved port).
        let appVersion = (Bundle.main.infoDictionary?["CFBundleVersion"] as? String) ?? ""
        if UserDefaults.standard.string(forKey: "webCacheVersion") != appVersion {
            let httpCacheTypes: Set<String> = [WKWebsiteDataTypeDiskCache, WKWebsiteDataTypeMemoryCache]
            // Mark done only AFTER the clear actually completes, so a kill mid-clear
            // retries on the next launch instead of skipping it forever.
            WKWebsiteDataStore.default().removeData(
                ofTypes: httpCacheTypes, modifiedSince: Date(timeIntervalSince1970: 0)
            ) {
                UserDefaults.standard.set(appVersion, forKey: "webCacheVersion")
            }
        }

        let root = ContentView().environmentObject(server)
        let host = UIHostingController(rootView: root)
        let win = UIWindow(frame: UIScreen.main.bounds)
        win.rootViewController = host
        win.makeKeyAndVisible()
        window = win
        server.start()
        return true
    }
}
