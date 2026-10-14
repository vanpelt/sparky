import SwiftUI
import VersoCore

/// Four tabs on paper, a floating glass pen, and the global sheets.
struct RootView: View {
    @Environment(AppModel.self) private var model
    @Namespace private var zoom
    @State private var keyboardUp = false

    /// The whole window (safe area added back), measured once here: every page's layout follows from it.
    @State private var window = CGSize(width: 402, height: 874)
    /// The window's safe area at rest (the keyboard's share left out): its side insets are where the system
    /// sets its bars down on the iPhone Duo.
    @State private var insets = EdgeInsets()
    /// Where a stack's page says the system presents bars vertically (`BarEdgeProbe`); nil before iOS 27.1.
    @State private var probedEdge: HorizontalEdge?

    private var layout: PageLayout { Measure.layout(for: window) }

    /// The window edge the system's bars stand on, nil while they run across the top and foot: the probe, or
    /// failing it a status-bar column on one side only (a phone held sideways has both). `-versoBarEdge` in Debug.
    private var barEdge: HorizontalEdge? {
        #if DEBUG
        if let e = DebugLaunch.barEdge { return e }
        #endif
        if let probedEdge { return probedEdge }
        if insets.trailing >= 60 && insets.leading < 8 { return .trailing }
        if insets.leading >= 60 && insets.trailing < 8 { return .leading }
        return nil
    }

    /// The bar column's width on `barEdge`. The `-versoFrame` harness has no real safe area, so there the
    /// column is the `-versoFrameInsets` margin on that edge, which RootView keeps clear itself (`debugColumn`).
    private var railWidth: CGFloat {
        guard let barEdge else { return 0 }
        return max(barEdge == .leading ? insets.leading : insets.trailing, debugColumn)
    }

    /// The tabs go down the side only where the system's own bars do, and there is a column to hold them.
    /// Everywhere else (every phone, the inner display upright, any build before 27.1) the running foot.
    private var showsRail: Bool { barEdge != nil && railWidth >= 56 }

    private var debugColumn: CGFloat {
        #if DEBUG
        if DebugLaunch.frame != nil, let e = DebugLaunch.barEdge {
            return e == .leading ? DebugLaunch.frameInsets.leading : DebugLaunch.frameInsets.trailing
        }
        #endif
        return 0
    }

    var body: some View {
        @Bindable var model = model
        ZStack {
            // Four stacks kept alive side by side (so each keeps its scroll and path), one shown at a time.
            ZStack {
                page(.contents) {
                    if layout.isSpread {
                        BookSplit(window: window, path: $model.contentsPath) {
                            ContentsView(zoom: zoom)
                        } recto: { route in
                            if let route { contentsPage(route, zoomed: false) } else { currentChapter }
                        }
                    } else {
                        NavigationStack(path: $model.contentsPath) {
                            ContentsView(zoom: zoom).paperGround()
                                .navigationDestination(for: ContentsRoute.self) { contentsPage($0, zoomed: true).paperGround() }
                        }
                    }
                }
                page(.ledger) {
                    NavigationStack { LedgerView().paperGround() }
                }
                page(.threads) {
                    if layout.isSpread {
                        BookSplit(window: window, path: $model.threadsPath) {
                            ThreadsView(zoom: zoom)
                        } recto: { id in
                            if let id { ThreadDetailView(threadID: id) } else { latestThread }
                        }
                    } else {
                        NavigationStack(path: $model.threadsPath) {
                            ThreadsView(zoom: zoom).paperGround()
                                .navigationDestination(for: UUID.self) { id in
                                    ThreadDetailView(threadID: id).paperGround()
                                        .navigationTransition(.zoom(sourceID: "thread-\(id)", in: zoom))
                                }
                        }
                    }
                }
                page(.turning) {
                    NavigationStack { TurningPointView().paperGround() }
                }
            }
            .fontWeight(.light)
            .padding(barEdge == .leading ? .leading : .trailing, debugColumn)
            .onPreferenceChange(BarEdgeKey.self) { edge in
                Task { @MainActor in probedEdge = edge }
            }
            // The pen belongs to the tab bar, not the keyboard, and you can't log mid-sentence anyway:
            // it steps aside while the keyboard is up (the layout lifts with the keyboard, so it can't stay put).
            // On a phone it floats off the text's trailing edge, as always. Anywhere else (the closed Duo's short
            // page, an open spread, a capped column) a floating pen lands on whatever line rests at the foot (the
            // Ledger's total, a clause's mark), so it is set in the running foot instead, beside the four tabs.
            .overlay(alignment: .bottomTrailing) {
                if !keyboardUp && layout.isPhone { PenButton().padding(.trailing, 20).padding(.bottom, 12).transition(.opacity) }
            }
            .safeAreaInset(edge: .bottom, spacing: 0) {
                if !keyboardUp && !showsRail {
                    ShelfBar(selection: $model.tab, isRecto: model.isRecto, carriesPen: !layout.isPhone).transition(.opacity)
                }
            }
            // Where the system sets its bars down the side (the closed Duo, the open one held landscape), the tabs
            // and pen go down the fore-edge with them, under the status bar and Back, and the page runs to the
            // foot. Outside every stack, so it rides through a spread being rebuilt. The keyboard takes the pen and
            // tabs away; the column itself is window-tall, hung from the top, so it never moves.
            .overlay(alignment: .top) {
                if showsRail, let barEdge {
                    // Only the width is flexible, reaching into the side safe area for the system's column. The
                    // height is the window's, hung from the top: with the keyboard up the page beneath is shorter,
                    // and a frame flexible in height would centre the column on it, 150pt up the window.
                    ForeEdge(selection: $model.tab, isRecto: model.isRecto, edge: barEdge, inset: railWidth,
                             height: window.height, bottomInset: insets.bottom, keyboardUp: keyboardUp)
                        .frame(maxWidth: .infinity, alignment: barEdge == .leading ? .leading : .trailing)
                        .ignoresSafeArea()
                        .transition(.opacity)
                }
            }
            // With no running foot, the page's last line at rest would sit under the home indicator: the paper
            // thickens under it, as at the foot of a printed page, and text scrolls into it rather than across it.
            .overlay(alignment: .bottom) {
                if showsRail && !keyboardUp && insets.bottom > 0 {
                    LinearGradient(stops: [.init(color: Palette.paper.opacity(0), location: 0),
                                           .init(color: Palette.paper.opacity(0.94), location: 0.7)],
                                   startPoint: .top, endPoint: .bottom)
                        .frame(height: insets.bottom + 6)
                        .padding(barEdge == .leading ? .leading : .trailing, debugColumn)
                        .ignoresSafeArea(edges: .bottom)
                        .allowsHitTesting(false)
                        .accessibilityHidden(true)
                        .transition(.opacity)
                }
            }
            .animation(.easeOut(duration: 0.15), value: keyboardUp)
            // Turning the open display: the running foot and the fore-edge cross-fade as the system turns.
            .animation(.easeInOut(duration: 0.25), value: showsRail)
            .onReceive(NotificationCenter.default.publisher(for: UIResponder.keyboardWillShowNotification)) { _ in keyboardUp = true }
            .onReceive(NotificationCenter.default.publisher(for: UIResponder.keyboardWillHideNotification)) { _ in keyboardUp = false }

            if model.needsOnboarding && model.titleCard == nil {
                OnboardingView()
                    .transition(.opacity.combined(with: .move(edge: .bottom)))
                    .zIndex(1)
            }

            if let card = model.titleCard {
                TitleCardView(script: card) { model.titleCard = nil }
                    .transition(.opacity)
                    .zIndex(2)
            }
        }
        .environment(\.pageLayout, layout)
        // Size plus insets is the window, and stays put while the keyboard trades one for the other.
        .onGeometryChange(for: CGSize.self) { g in
            CGSize(width: g.size.width + g.safeAreaInsets.leading + g.safeAreaInsets.trailing,
                   height: g.size.height + g.safeAreaInsets.top + g.safeAreaInsets.bottom)
        } action: { size in
            // Folding keeps both paths: what was on the right page is pushed in one column, and the reverse.
            let spread = Measure.layout(for: size).isSpread
            if model.isSpread != spread, model.pages.focus != nil {
                // The pages are about to be rebuilt under the field that has the keyboard. Let it go first, while
                // the field is still in the window, and mark it as going: its twin in the new tree takes the
                // keyboard back (`keepsFocus`). A field torn out of the window while still first responder left
                // the app reporting animations in flight long after.
                model.pages.focusLeftAt = Date()
                UIApplication.shared.sendAction(#selector(UIResponder.resignFirstResponder), to: nil, from: nil, for: nil)
            }
            window = size
            if model.isSpread != spread { model.isSpread = spread }
        }
        .onGeometryChange(for: EdgeInsets.self) { $0.safeAreaInsets } action: { i in
            // The keyboard lifts the bottom inset: keep the one at rest, which is the home indicator's.
            if !keyboardUp { insets = i } else { insets.leading = i.leading; insets.trailing = i.trailing }
        }
        .gesture(TwoFingerTap { withAnimation(.easeOut(duration: 0.45)) { model.veilLifted.toggle() } })
        .sheet(item: $model.sheet) { sheet in
            SheetHost(sheet: sheet)
                .environment(model)
                .environment(\.versoVeiled, model.veiled)
        }
        .sensoryFeedback(Haptics.log, trigger: model.logCount)
        .sensoryFeedback(Haptics.target, trigger: model.successCount)
        .onChange(of: model.route) { _, r in
            guard let r else { return }
            model.handle(r)
            model.route = nil
        }
        .background(Palette.paper.ignoresSafeArea())
    }

    /// One Contents destination. The zoom from its source in the list belongs to a push; on a spread the right
    /// page simply turns.
    @ViewBuilder func contentsPage(_ route: ContentsRoute, zoomed: Bool) -> some View {
        switch route {
        case .chapter(let i):
            ChapterDetailView(index: i).zoomed(zoomed, from: "chapter-\(i)", in: zoom)
        case .thread(let id):
            ThreadDetailView(threadID: id).zoomed(zoomed, from: "spine-\(id)", in: zoom)
        case .colophon:
            ColophonView()
        #if DEBUG
        case .widgetGallery:
            WidgetGalleryView(live: SnapshotBuilder.build(from: model.board),
                              startLive: UserDefaults.standard.bool(forKey: "versoGalleryLive"),
                              anchor: DebugLaunch.scrollAnchor)
        #endif
        }
    }

    /// Contents' right page with nothing chosen: the book falls open at the current chapter.
    @ViewBuilder var currentChapter: some View {
        if let i = (model.board.ledger ?? model.scheduleLedger)?.currentIndex {
            ChapterDetailView(index: i).id(i)
        } else {
            BlankRecto(note: "Set the grant in the Colophon, and the chapters open here.")
        }
    }

    /// Threads' right page with nothing chosen: the thread most recently written in (the first, if none yet).
    @ViewBuilder var latestThread: some View {
        // The same pick the list marks as open on the left page.
        if let latest = ThreadsView.lastLogged(model.board.threads) {
            ThreadDetailView(threadID: latest).id(latest)
        } else {
            BlankRecto(note: "A thread's scenes open here.")
        }
    }
}

extension RootView {
    /// One tab's stack: always alive, visible and hittable only when selected.
    @ViewBuilder func page<Content: View>(_ tab: AppTab, @ViewBuilder _ content: () -> Content) -> some View {
        let on = model.tab == tab
        content()
            .opacity(on ? 1 : 0)
            .allowsHitTesting(on)
            .accessibilityHidden(!on)
            .environment(\.tabShown, on)
    }
}

extension EnvironmentValues {
    /// Whether this view's tab is the one showing (`RootView.page`). The hidden tabs stay in the accessibility
    /// tree whatever `.accessibilityHidden` says (seen on iOS 27), so a page names itself only while it shows.
    @Entry var tabShown = true
}

extension View {
    /// Paper as the navigation stack's own ground, under every page it shows. On the open Duo (iOS 27.1) a stack
    /// runs on under the status bar's column at the window's trailing edge, past the reach of a page's own
    /// background, and showed white there.
    /// It also asks the page where the system sets its bars (`BarEdgeProbe`), for the fore-edge.
    func paperGround() -> some View {
        containerBackground(Palette.paper, for: .navigation).background(BarEdgeProbe())
    }
}

private extension View {
    /// The zoom out of a list row, for a destination that is pushed; none for one that opens on a facing page.
    @ViewBuilder func zoomed(_ on: Bool, from sourceID: String, in zoom: Namespace.ID) -> some View {
        if on { navigationTransition(.zoom(sourceID: sourceID, in: zoom)) } else { self }
    }
}

/// Contents and Threads on an open spread: the list on the left page, what's open from it on the right.
///
/// Both pages work the same path as the one-column stack, so folding and unfolding keep what's open. The path's
/// first entry is the right page itself, with no back button (the list beside it is the way out); anything pushed
/// from there stacks above it as usual. A row tapped on the left turns the right page (`path = [route]`), however
/// the row asks for it (`model.openContents` / `openThread`).
///
/// The left page has no stack of its own. On iOS 27.1 every navigation stack keeps a column at its trailing edge for
/// vertical bars (the open Duo's side bars), even with its bar hidden. A stack on the left page left an 84pt blank
/// strip down the middle of the spread. Only the right page's stack reaches the window edge, where that column
/// belongs to the status bar anyway.
///
/// It splits at the WINDOW midline and keeps a `Spread`'s margins, 28pt outside and 14pt each side of the spine,
/// so the text sits in the same place on every tab. Each page is a single `Page` with its 28pt gutters, so each is
/// laid out 14pt wider than its half, reaching under the spine, and cut back at the midline (that strip is only
/// margin). The stacks themselves stay exactly half: their bars and buttons are never cut.
struct BookSplit<Route: Hashable, Verso: View, Recto: View>: View {
    let window: CGSize
    @Binding var path: [Route]
    let verso: Verso
    /// The right page for a route; nil is the page the book falls open at when nothing is chosen.
    let recto: (Route?) -> Recto
    @Environment(\.tabShown) private var tabShown

    /// How far the two pages' gutters overlap across the spine (a single page's 28 less a spread's 14, twice).
    private static var overlap: CGFloat { 28 }

    init(window: CGSize, path: Binding<[Route]>, @ViewBuilder verso: () -> Verso,
         @ViewBuilder recto: @escaping (Route?) -> Recto) {
        self.window = window; self._path = path; self.verso = verso(); self.recto = recto
    }

    var body: some View {
        GeometryReader { geo in
            let inset = geo.safeAreaInsets
            let half = window.width / 2, lap = Self.overlap / 2
            // Each page is laid out as a single page as wide as its frame, gutter and measure alike.
            let pageLayout = Measure.layout(for: CGSize(width: half + lap, height: window.height))
            HStack(spacing: 0) {
                verso.underSpine(trailing: lap)
                    .frame(width: max(0, half - inset.leading))
                    .environment(\.bookPage, .verso)
                    .accessibilityElement(children: .contain)
                    .accessibilityIdentifier(tabShown ? "page.verso" : "")

                NavigationStack(path: pushed) {
                    recto(path.first).id(path.first).underSpine(leading: lap).paperGround()
                        .navigationDestination(for: Route.self) { recto($0).underSpine(leading: lap).paperGround() }
                }
                .frame(width: max(0, half - inset.trailing))
                .environment(\.bookPage, .recto)
                .accessibilityElement(children: .contain)
                .accessibilityIdentifier(tabShown ? "page.recto" : "")
            }
            .environment(\.pageLayout, pageLayout)
            .overlay(alignment: .topLeading) {
                Spine().offset(x: half - inset.leading - Spine.width / 2)
            }
        }
    }

    /// The right page's stack: everything after the first entry, which is its root.
    private var pushed: Binding<[Route]> {
        Binding { Array(path.dropFirst()) } set: { rest in
            if let first = path.first { path = [first] + rest } else { path = rest }
        }
    }
}

/// Only the sides are cut: paper still runs up under the bars and down past the foot.
private struct SideClip: Shape {
    func path(in r: CGRect) -> Path { Path(r.insetBy(dx: 0, dy: -2000)) }
}

private extension View {
    /// Lays a page out `leading`/`trailing` wider, reaching under the spine, then cuts it back to its own half.
    func underSpine(leading: CGFloat = 0, trailing: CGFloat = 0) -> some View {
        padding(.leading, -leading).padding(.trailing, -trailing)
            .clipShape(SideClip())
            .contentShape(SideClip())
    }
}

/// A right page with nothing to show yet.
private struct BlankRecto: View {
    let note: String
    var body: some View {
        Text(note).font(VersoFont.his(17)).foregroundStyle(Palette.fadedInk).multilineTextAlignment(.center)
            .padding(40)
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .background(Palette.paper.ignoresSafeArea())
    }
}

/// The tab bar, set like a book's running foot: four small-caps words on paper under a hairline. The
/// selected one is in oxblood with a short rule beneath; the rest in faded ink. No pills, no bubbles.
struct ShelfBar: View {
    @Binding var selection: AppTab
    let isRecto: Bool
    /// Off a phone the pen is set at the foot's trailing end (RootView), a little smaller, so it never rests on
    /// the page's last line. The labels close up a touch to make room on the narrow closed Duo.
    var carriesPen = false

    var body: some View {
        VStack(spacing: 0) {
            Hairline()
            HStack(spacing: 0) {
                ForEach(Self.items(isRecto: isRecto)) { item($0.tab, $0.title, $0.symbol) }
                if carriesPen { PenButton(size: 46).padding(.leading, 6) }
            }
            .padding(.horizontal, 8)
            .padding(.top, 6)
            // A running foot as wide as the text, not the window: the rule still runs the width of the page.
            .frame(maxWidth: Measure.text)
            .frame(maxWidth: .infinity)
        }
        .background(Palette.paper.ignoresSafeArea(edges: .bottom))
    }

    /// The four tabs, shared with the fore-edge (`ForeEdge`). Turning Point is The Turn once the book is in recto.
    struct Item: Identifiable {
        let tab: AppTab, title: String, symbol: String
        var id: AppTab { tab }
    }

    static func items(isRecto: Bool) -> [Item] {
        [Item(tab: .contents, title: "Contents", symbol: "book.closed"),
         Item(tab: .ledger, title: "Ledger", symbol: "text.book.closed"),
         Item(tab: .threads, title: "Threads", symbol: "scribble"),
         Item(tab: .turning, title: isRecto ? "The Turn" : "Turning Point", symbol: "scope")]
    }

    func item(_ tab: AppTab, _ title: String, _ symbol: String) -> some View {
        let on = selection == tab
        return Button {
            if !on { selection = tab }
        } label: {
            VStack(spacing: 4) {
                Image(systemName: symbol).font(.system(size: 18, weight: on ? .regular : .light))
                Text(title.uppercased()).font(VersoFont.body(9.5)).tracking(carriesPen ? 1 : 1.4).lineLimit(1)
                    .minimumScaleFactor(0.8)
                Capsule().frame(width: 16, height: 1.5).opacity(on ? 1 : 0)
            }
            .foregroundStyle(on ? Palette.oxblood : Palette.fadedInk)
            .frame(maxWidth: .infinity, minHeight: 50)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .sensoryFeedback(.selection, trigger: on)
        .accessibilityLabel(title)
        .accessibilityAddTraits(on ? [.isButton, .isSelected] : .isButton)
    }
}

/// The floating ✒︎: tap → Quick Log (pick a thread); long-press → Marginalia.
struct PenButton: View {
    /// 54pt floating on a phone; smaller where it sits in the running foot.
    var size: CGFloat = 54
    @Environment(AppModel.self) private var model
    var body: some View {
        Image(systemName: "pencil.tip")
            .font(.system(size: 21 * size / 54, weight: .light))
            .foregroundStyle(Palette.ink)
            .frame(width: size, height: size)
            .glassEffect(.regular.interactive(), in: .circle)
            .contentShape(Circle())
            .onTapGesture { model.sheet = .log(nil) }
            .onLongPressGesture(minimumDuration: 0.4) { model.sheet = .marginalia }
            .sensoryFeedback(.impact(weight: .light), trigger: model.sheet)
            .accessibilityLabel("Log")
            .accessibilityHint("Long-press for Marginalia")
            .accessibilityAddTraits(.isButton)
            .accessibilityAction(named: "Marginalia") { model.sheet = .marginalia }
    }
}

/// Builds each global sheet with its detents. On a regular-width window the editors are form sheets and the
/// long reads (Marginalia, closing the book) page sheets; a phone's sheets ignore the sizing and are unchanged.
struct SheetHost: View {
    let sheet: AppSheet
    @Environment(AppModel.self) private var model
    var body: some View {
        switch sheet {
        case .marginalia:
            MarginaliaSheet().presentationDetents([.large]).presentationBackground(Palette.paper).presentationSizing(.page)
        case .log(let id):
            QuickLogSheet(threadID: id).presentationSizing(.form)
        case .newThread:
            ThreadEditor(threadID: nil).presentationBackground(Palette.paper).presentationSizing(.form)
        case .editThread(let id):
            ThreadEditor(threadID: id).presentationBackground(Palette.paper).presentationSizing(.form)
        case .price:
            // .medium left half the sheet blank below Refresh/Done.
            PriceSheet().presentationDetents([.height(400)]).presentationBackground(Palette.paper).presentationSizing(.form)
        case .newClause:
            ClauseEditor(clauseID: nil).presentationBackground(Palette.paper).presentationSizing(.form)
        case .editClause(let id):
            ClauseEditor(clauseID: id).presentationBackground(Palette.paper).presentationSizing(.form)
        case .closeBook:
            CloseBookFlow().presentationDetents([.large]).presentationBackground(Palette.paper).presentationSizing(.page)
        case .editEntry(let id):
            if let e = model.state.entries.first(where: { $0.id == id }) {
                EntryEditor(entry: e).presentationDetents([.height(300)]).presentationBackground(Palette.paper)
            }
        }
    }
}

#Preview {
    RootView()
        .environment(AppModel.preview)
        .environment(\.versoVeiled, false)
}

