import SwiftUI
import UIKit

struct ContentView: View {
    @EnvironmentObject var server: ServerController

    var body: some View {
        ZStack {
            // Background fills the notch + home-indicator area with the
            // same color as the page so there's no visible band; the
            // WebView itself stays *inside* the safe area so the page's
            // CSS env(safe-area-inset-*) returns 0 and we don't end up
            // double-padding (system inset + body padding).
            //
            // Color(.sRGB, red:…) and edgesIgnoringSafeArea(.all) are the
            // iOS-13-compatible spellings of Color(red:…) / ignoresSafeArea().
            Color(.sRGB, red: 0.07, green: 0.09, blue: 0.13, opacity: 1)
                .fillSafeArea()
            if server.port > 0 {
                WebView(url: URL(string: "http://127.0.0.1:\(server.port)")!)
                    // Force a fresh WebView (and page reload) on every server
                    // restart, even when the port is unchanged. See
                    // ServerController.generation.
                    .id(server.generation)
                    .ignoreKeyboardSafeArea()
            } else if let err = server.lastError {
                VStack(spacing: 12) {
                    Text("startup failed").font(.headline).foregroundColor(.white)
                    Text(err).font(.caption).foregroundColor(.secondary)
                    Button("retry") { server.start() }
                }
                .padding()
            } else {
                LoadingSpinner()
            }
        }
    }
}

// ProgressView is iOS 14+; fall back to a UIKit spinner on iOS 13.
private struct LoadingSpinner: View {
    var body: some View {
        if #available(iOS 14.0, *) {
            ProgressView()
        } else {
            ActivityIndicator()
        }
    }
}

private struct ActivityIndicator: UIViewRepresentable {
    func makeUIView(context: Context) -> UIActivityIndicatorView {
        let v = UIActivityIndicatorView(style: .large)
        v.startAnimating()
        return v
    }
    func updateUIView(_ view: UIActivityIndicatorView, context: Context) {}
}

private extension View {
    // ignoresSafeArea() is iOS 14+; fall back to edgesIgnoringSafeArea on 13.
    @ViewBuilder func fillSafeArea() -> some View {
        if #available(iOS 14.0, *) {
            self.ignoresSafeArea()
        } else {
            self.edgesIgnoringSafeArea(.all)
        }
    }

    // ignoresSafeArea(.keyboard) is iOS 14+; iOS 13 has no automatic SwiftUI
    // keyboard avoidance to opt out of, so it's a no-op there.
    @ViewBuilder func ignoreKeyboardSafeArea() -> some View {
        if #available(iOS 14.0, *) {
            self.ignoresSafeArea(.keyboard)
        } else {
            self
        }
    }
}
