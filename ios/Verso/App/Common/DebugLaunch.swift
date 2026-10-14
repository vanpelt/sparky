import SwiftUI
import VersoCore

#if DEBUG
/// Launch arguments for screenshotting without tapping (`simctl launch <udid> <VERSO_ID> -key value`).
///
/// - `-versoSkipOnboarding YES` hides onboarding for this run only (nothing is written to the store).
/// - `-versoFirstRun YES` runs on a throwaway EMPTY store, as a fresh install would (onboarding and all).
/// - `-versoDayOne YES` runs on a throwaway store in the state onboarding leaves: four empty threads, one
///   clause, no Marginalia (and the bundled seed as the grant, if there is one).
/// - `-versoSample YES` / `-versoSeededSample YES`: throwaway stores full of invented history (see AppModel).
/// - `-versoOnboardingStep 0|1|2` opens onboarding on a given card.
/// - `-versoScroll 0.0…1.0` starts every page scrolled to that fraction (1 = bottom).
/// - `-versoGalleryLive YES` opens the Widget Gallery on Live data.
/// - `-versoGalleryProfile "Duo outer"|"Duo inner"` opens the Widget Gallery on that slot profile.
/// - `-versoScreen gallery|price|quicklog|newThread|editThread|newClause|editClause|closeBook|marginalia|thread`
///   opens a debug destination after launch (`thread` is the first thread). Tabs and chapters: `-versoRoute`.
/// - `-versoFrame WxH` (e.g. `951x669`) sets the whole app in a fixed, centred window of that size, so an iPad
///   simulator can stand in for any iPhone Duo window. See `DebugFrame`.
/// - `-versoFrameInsets top,leading,bottom,trailing` margins standing in for the frame's safe area (default
///   `24,0,20,0`, a status bar and a home indicator; the Duo's real values are unpublished).
/// - `-versoFoldTo WxH` (with `-versoFrame`) adds a fold button in the deckle's leading margin
///   (accessibility identifier `debug.fold`) that swaps the frame between the two sizes in place, as folding and
///   unfolding the Duo resizes a running app.
/// - `-versoBarEdge trailing|leading` takes the system's bars as set down that side, as `toolbarVerticalEdge`
///   reports on the Duo, so the tabs go down the fore-edge (`ForeEdge`). With `-versoFrame` the column is that
///   edge's `-versoFrameInsets` margin (`-versoFrameInsets 0,0,20,84` for the Duo's ~84pt).
enum DebugLaunch {
    private static var defaults: UserDefaults { .standard }
    static var skipOnboarding: Bool { defaults.bool(forKey: "versoSkipOnboarding") }
    static var onboardingStep: Int { defaults.integer(forKey: "versoOnboardingStep") }
    static var screen: String? { defaults.string(forKey: "versoScreen") }
    static var scrollAnchor: UnitPoint? {
        guard let s = defaults.string(forKey: "versoScroll"), let y = Double(s) else { return nil }
        return UnitPoint(x: 0.5, y: min(max(y, 0), 1))
    }
    static var frame: CGSize? { size("versoFrame") }
    /// The frame's paper margins: `frameInsets`, less the bar column, which RootView keeps clear itself as the
    /// safe area is on the device.
    static var frameMargins: EdgeInsets {
        var m = frameInsets
        if barEdge == .leading { m.leading = 0 }
        if barEdge == .trailing { m.trailing = 0 }
        return m
    }
    static var barEdge: HorizontalEdge? {
        switch defaults.string(forKey: "versoBarEdge") {
        case "trailing": .trailing
        case "leading": .leading
        default: nil
        }
    }
    static var foldTo: CGSize? { size("versoFoldTo") }
    private static func size(_ key: String) -> CGSize? {
        guard let s = defaults.string(forKey: key) else { return nil }
        let wh = s.lowercased().split(separator: "x").compactMap { Double($0) }
        guard wh.count == 2, wh[0] > 0, wh[1] > 0 else { return nil }
        return CGSize(width: wh[0], height: wh[1])
    }
    static var frameInsets: EdgeInsets {
        let v = (defaults.string(forKey: "versoFrameInsets") ?? "").split(separator: ",").compactMap { Double($0) }
        guard v.count == 4 else { return EdgeInsets(top: 24, leading: 0, bottom: 20, trailing: 0) }
        return EdgeInsets(top: v[0], leading: v[1], bottom: v[2], trailing: v[3])
    }
}

/// `-versoFrame`: the app as if its window were `size`, centred on deckle with a hairline round it.
///
/// The real safe area is shut out (the frame sits in the middle of the screen anyway) and `-versoFrameInsets`
/// set as plain paper margins instead: a safe area added in SwiftUI never reaches the UIKit-hosted navigation
/// stacks (their pages laid out as if the inset were 0), but a margin takes the same height off every page, so the
/// first screen holds what it would on the device. Paper fills the margins; there is no status bar to fade under.
/// Size classes follow the frame, as the Duo reports them: regular wide only on the inner display (both sides
/// at least 600), compact tall below 500. The keyboard's safe area stays real, so fields still lift.
/// What it can't show: system bars, sheets (they present from the real window), and the fold itself, though
/// `foldTo` resizes the frame in place as folding does.
struct DebugFrame: ViewModifier {
    /// The `-versoFrame` size the app launched in.
    let launched: CGSize
    let insets: EdgeInsets
    /// The other size the fold button swaps to (`-versoFoldTo`); no button without one.
    var foldTo: CGSize?
    @State private var folded = false

    func body(content: Content) -> some View {
        let size = folded ? foldTo ?? launched : launched
        content
            .environment(\.horizontalSizeClass, size.width >= 600 && size.height >= 600 ? .regular : .compact)
            .environment(\.verticalSizeClass, size.height < 500 ? .compact : .regular)
            .padding(insets)
            .frame(width: size.width, height: size.height)
            .background(Palette.paper)
            .clipped()
            .overlay { Rectangle().strokeBorder(Palette.rule, lineWidth: 0.5).allowsHitTesting(false) }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .background(Palette.deckle)
            // Half way down the leading margin: with the keyboard up the frame rides up past the top of the
            // screen, and its middle is what stays in view.
            .overlay(alignment: .leading) {
                if let foldTo {
                    let next = folded ? launched : foldTo
                    Button { folded.toggle() } label: {
                        Text("⇆ \(Int(next.width))×\(Int(next.height))").font(.caption2.monospacedDigit())
                            .foregroundStyle(Palette.fadedInk)
                            .padding(10).contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                    .padding(.leading, 4)
                    .accessibilityIdentifier("debug.fold")
                }
            }
            .ignoresSafeArea(.container)
    }
}
#endif

extension View {
    /// Applies `-versoFrame` in Debug builds; a no-op in Release and when the argument is absent.
    @ViewBuilder func debugFrame() -> some View {
        #if DEBUG
        if let size = DebugLaunch.frame {
            modifier(DebugFrame(launched: size, insets: DebugLaunch.frameMargins, foldTo: DebugLaunch.foldTo))
        } else { self }
        #else
        self
        #endif
    }
}

extension View {
    /// Applies `-versoScroll` in Debug builds; a no-op in Release.
    func debugScrollAnchor() -> some View {
        #if DEBUG
        return defaultScrollAnchor(DebugLaunch.scrollAnchor)
        #else
        return self
        #endif
    }
}
