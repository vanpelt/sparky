import SwiftUI
import UniformTypeIdentifiers
import VersoCore

/// The Colophon (settings), typeset like the last page of a book.
struct ColophonView: View {
    @Environment(AppModel.self) private var model
    @State private var grant = Grant(revestShares: 0, rsuShares: 0)
    @State private var hasGrant = false
    @State private var settings = Settings()
    @State private var loaded = false
    @State private var dirty = false
    @State private var confirmReimport = false
    @State private var confirmErase = false
    @State private var importing = false
    @State private var exportURL: URL?
    @State private var slug: Slug?
    @State private var override: Double?
    @State private var overrideOn = false
    @State private var marginaliaTime = Date()

    struct Slug { var snapshot: Date?; var reload: Date? }

    var body: some View {
        Page(folio: model.board.folio) {
            HeadBar("COLOPHON")
        } content: {
            Text(imprint).font(VersoFont.his(17)).foregroundStyle(Palette.ink)
                .fixedSize(horizontal: false, vertical: true)
                .padding(.vertical, 18)

            DinkusRule()
            grantSection
            DinkusRule()
            taxesSection
            DinkusRule()
            paySection
            DinkusRule()
            enoughSection
            DinkusRule()
            priceSection
            DinkusRule()
            privacySection
            DinkusRule()
            marginaliaSection
            DinkusRule()
            dataSection
            #if DEBUG
            DinkusRule()
            SectionTitle("The proofs", glyph: "✂︎")
            NavigationLink(value: ContentsRoute.widgetGallery) {
                Text("Widget Gallery ›").font(VersoFont.serif(.body)).italic().foregroundStyle(Palette.oxblood)
            }
            #endif
            DinkusRule()
            slugView
        }
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            if dirty {
                ToolbarItem(placement: .confirmationAction) {
                    Button { saveGrant() } label: { Text("Set in type").font(VersoFont.serif(.body)).italic() }.tint(Palette.oxblood)
                }
            }
        }
        .onAppear(perform: load)
        .onDisappear {
            if dirty { saveGrant() }
            // Closed, not folded (a fold keeps the route): the next opening starts from the book.
            if !model.contentsPath.contains(.colophon) { model.pages.colophon = nil }
        }
        .onChange(of: grant) { _, _ in if loaded { dirty = true } }
        // The form as typed is mirrored into the model, and a rebuilt Colophon (a fold) picks it up from there
        // instead of reading back a book its own save hasn't reached yet.
        .onChange(of: form) { _, f in if loaded { model.pages.colophon = f } }
        .onChange(of: settings) { _, s in saveSettings(s) }
        .fileImporter(isPresented: $importing, allowedContentTypes: [.json]) { result in
            guard case .success(let url) = result else { return }
            let ok = url.startAccessingSecurityScopedResource()
            defer { if ok { url.stopAccessingSecurityScopedResource() } }
            guard let data = try? Data(contentsOf: url) else { return }
            model.perform { try await $0.importJSON(data) }
            reload()
        }
        .task { await refreshSlug(); await makeExport() }
    }

    var imprint: String {
        let p = model.state.price
        let src = p.isManual ? "manual" : "\(p.source ?? "no source") · \(p.ageLabel(now: model.now()))"
        return "Set in New York, Didot & Iowan Old Style. Printed on \(model.state.book.settings.ticker) at \(MoneyFormat.price(p.effective)) (\(src)) on \(DateText.long(model.now()))."
    }

    // MARK: Sections

    var grantSection: some View {
        VStack(alignment: .leading, spacing: 4) {
            SectionTitle("The grant")
            if !hasGrant {
                Aside("No grant yet. Enter your figures, or use the sample.")
                QuietButton("Use sample numbers", color: Palette.oxblood) {
                    model.perform { try await $0.useSampleGrant() }
                    reload()
                }
            } else if grant.isSample {
                Text("These are sample figures.").font(VersoFont.his(15)).foregroundStyle(Palette.rust)
            }
            NumberRow(label: "Revest shares", value: $grant.revestShares)
            NumberRow(label: "RSU shares", value: $grant.rsuShares)
            NumberRow(label: "Rollover shares", value: $grant.rolloverShares)
            PaperDateRow(label: "Rollover vests", selection: Binding { grant.rolloverVestDate ?? defaultRolloverDate } set: { grant.rolloverVestDate = $0 })
            Aside("A separate rollover grant, already yours: it vests on this one date and is kept out of every \u{201C}left to vest\u{201D} figure. It is not part of the quarterly schedule below.")
            if grant.rolloverVestDate == nil { Aside("Unknown — treated as \u{201C}2026\u{201D}.") }
            Stepper("Vest day of month: \(grant.vestDayOfMonth)", value: $grant.vestDayOfMonth, in: 1...31)
                .font(VersoFont.body(16))
            Aside("Revest and RSU shares vest together on this day — the cliff and then quarterly. They differ only in tax: revest at the long-term rate, RSUs at ordinary income (see Taxes).")
            PaperDateRow(label: "Book One began", selection: Binding { grant.bookOneStart ?? model.state.book.state.firstLaunchAt } set: { grant.bookOneStart = $0 })
            Aside("Drives the folio — p. \(model.board.folio).")
            ScheduleEditor(schedule: $grant.schedule).padding(.top, 8)
        }
    }

    var defaultRolloverDate: Date { Days.date(year: 2026, month: 12, day: 15, calendar: model.calendar) }

    var taxesSection: some View {
        VStack(alignment: .leading, spacing: 4) {
            SectionTitle("Taxes")
            NumberRow(label: "Revest (long-term)", value: $grant.taxRevest, format: .percent)
            PaperToggle(label: "Revest sheltered by loss carryforward", isOn: $grant.revestSheltered)
            if grant.revestSheltered { Aside("Sheltered by your loss carryforward, so revest is taxed at 0% (as in futr).") }
            NumberRow(label: "Ordinary income", value: $grant.taxOrdinary, format: .percent)
            Aside("Applied to RSUs, the rollover, salary and bonus.")
        }
    }

    var paySection: some View {
        VStack(alignment: .leading, spacing: 4) {
            SectionTitle("Pay")
            NumberRow(label: "Annual salary", value: $grant.salaryAnnual, format: .money)
            NumberRow(label: "Annual bonus", value: $grant.bonusAnnual, format: .money)
            Aside("Both stop the day you leave.")
        }
    }

    var enoughSection: some View {
        VStack(alignment: .leading, spacing: 4) {
            SectionTitle("The Enough Line")
            Text("What would you pay to have one Tuesday back?").font(VersoFont.his(17))
            OptionalMoneyRow(label: "Worth staying for", value: $settings.enoughLinePerDay, suffix: "/day")
        }
    }

    var priceSection: some View {
        VStack(alignment: .leading, spacing: 4) {
            SectionTitle("Price")
            HStack {
                Text("Ticker").font(VersoFont.body(16))
                Spacer()
                TextField("CRWV", text: $settings.ticker)
                    .textInputAutocapitalization(.characters).autocorrectionDisabled()
                    .multilineTextAlignment(.trailing).font(VersoFont.ledger(.body)).foregroundStyle(Palette.oxblood)
            }
            .padding(.vertical, 4)
            OptionalMoneyRow(label: "Manual price", value: $override)
                .onSubmit(saveOverride)
            PaperToggle(label: "Use the manual price", isOn: $overrideOn)
                .disabled(override == nil)
                .onChange(of: overrideOn) { _, _ in saveOverride() }
            QuietButton("Refresh the live price", color: Palette.oxblood) { Task { await model.refreshPrice(force: true) } }
        }
    }

    var privacySection: some View {
        VStack(alignment: .leading, spacing: 4) {
            SectionTitle("Privacy")
            PaperToggle(label: "Veil figures by default", isOn: $settings.veilByDefault)
            PaperToggle(label: "Show figures on the Lock Screen", isOn: $settings.showFiguresOnLockScreen)
            Aside("Two fingers on any page lifts the veil until you leave the app.")
        }
    }

    var marginaliaSection: some View {
        VStack(alignment: .leading, spacing: 4) {
            SectionTitle("Marginalia")
            PaperToggle(label: "Remind me each week", isOn: $settings.notificationsOn)
            PickerRow("On") {
                Picker("Day", selection: $settings.marginaliaWeekday) {
                    ForEach(1...7, id: \.self) { d in Text(model.calendar.weekdaySymbols[d - 1]).tag(d) }
                }
                .font(VersoFont.body(16)).tint(Palette.oxblood)
            }
            PaperDateRow(label: "At", selection: $marginaliaTime, components: .hourAndMinute)
                .onChange(of: marginaliaTime) { _, t in settings.marginaliaHour = model.calendar.component(.hour, from: t) }
        }
    }

    var dataSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            SectionTitle("Data")
            Text(seedStatus).font(VersoFont.his(15)).foregroundStyle(Palette.fadedInk)
            if SeedFile.isBundled() {
                QuietButton("Re-import seed", color: Palette.oxblood) { confirmReimport = true }
                    .confirmationDialog("Overwrite the grant with the bundled seed?", isPresented: $confirmReimport, titleVisibility: .visible) {
                        Button("Re-import seed", role: .destructive) {
                            model.perform { _ = try await $0.reimportSeed() }
                            reload()
                        }
                    } message: { Text("Only the grant is replaced. Threads, clauses and Marginalia stay.") }
            }
            if let exportURL {
                ShareLink(item: exportURL) {
                    Text("EXPORT JSON").runningHeadStyle(Palette.oxblood, size: 12).padding(.vertical, 6)
                }
            }
            QuietButton("Import JSON", color: Palette.oxblood) { importing = true }
            QuietButton("Erase everything", color: Palette.rust) { confirmErase = true }
                .confirmationDialog("Erase every page of Verso?", isPresented: $confirmErase, titleVisibility: .visible) {
                    Button("Erase everything", role: .destructive) {
                        model.perform { try await $0.eraseAll() }
                        model.contentsPath = []
                    }
                } message: { Text("Export first if you want to keep anything. This can't be undone.") }
        }
    }

    var seedStatus: String {
        if let at = model.state.book.state.seedImportedAt {
            return "Imported from local seed · \(at.formatted(.dateTime.day().month(.abbreviated)))"
        }
        return SeedFile.isBundled() ? "Seed in bundle, not imported" : "No seed in bundle"
    }

    var slugView: some View {
        let v = Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "?"
        let b = Bundle.main.infoDictionary?["CFBundleVersion"] as? String ?? "?"
        let fmt: (Date?) -> String = { $0.map { $0.formatted(.dateTime.day().month(.abbreviated).hour().minute().second()) } ?? "—" }
        return VStack(alignment: .leading, spacing: 3) {
            Text("VERSO \(v) (\(b))")
            Text("group \(AppGroup.isShared ? AppGroup.identifier : "unavailable — local store")")
            Text(AppGroup.storeDirectory.path(percentEncoded: false)).lineLimit(2).truncationMode(.middle)
            Text("snapshot \(fmt(slug?.snapshot)) · widgets \(fmt(slug?.reload))")
        }
        .font(VersoFont.slug).foregroundStyle(Palette.fadedInk)
        .textSelection(.enabled)
    }

    // MARK: Load / save

    var form: ColophonForm {
        ColophonForm(grant: grant, hasGrant: hasGrant, settings: settings, override: override, overrideOn: overrideOn,
                     marginaliaTime: marginaliaTime, dirty: dirty)
    }

    func load() {
        guard !loaded else { return }
        let s = model.state
        let f = model.pages.colophon ?? ColophonForm(
            grant: s.book.grant ?? Grant(revestShares: 0, rsuShares: 0), hasGrant: s.book.grant != nil,
            settings: s.book.settings, override: s.price.overridePrice, overrideOn: s.price.overrideEnabled,
            marginaliaTime: model.calendar.date(bySettingHour: s.book.settings.marginaliaHour, minute: 0, second: 0,
                                                of: model.now()) ?? model.now(),
            dirty: false)
        hasGrant = f.hasGrant; grant = f.grant; settings = f.settings
        override = f.override; overrideOn = f.overrideOn; marginaliaTime = f.marginaliaTime
        DispatchQueue.main.async { loaded = true; dirty = f.dirty; model.pages.colophon = form }
    }

    /// Reads the form back from the book once a replacement (import, seed, sample) has landed.
    func reload() {
        model.pages.colophon = nil
        loaded = false
        Task { try? await Task.sleep(for: .milliseconds(300)); load() }
    }

    func saveGrant() {
        let g = grant
        dirty = false; hasGrant = true
        model.pages.colophon?.dirty = false; model.pages.colophon?.hasGrant = true
        var stamped = g; stamped.isSample = g.isSample && g == model.state.book.grant
        let final = stamped
        model.perform { try await $0.setGrant(final) }
        Task { try? await Task.sleep(for: .milliseconds(300)); Notifications.reschedule(for: model.state, calendar: model.calendar) }
    }

    func saveSettings(_ s: Settings) {
        guard loaded, s != model.state.book.settings else { return }
        let reschedule = s.notificationsOn != model.state.book.settings.notificationsOn
            || s.marginaliaWeekday != model.state.book.settings.marginaliaWeekday
            || s.marginaliaHour != model.state.book.settings.marginaliaHour
        let tickerChanged = s.ticker != model.state.book.settings.ticker
        model.perform { store in
            try await store.updateSettings { $0 = s }
            if tickerChanged { _ = try await store.refreshPrice(force: true) }
        }
        if reschedule {
            Task {
                if s.notificationsOn { _ = await Notifications.requestAuthorization() }
                try? await Task.sleep(for: .milliseconds(300))
                Notifications.reschedule(for: model.state, calendar: model.calendar)
            }
        }
    }

    func saveOverride() {
        let o = override, on = overrideOn && override != nil
        guard o != model.state.price.overridePrice || on != model.state.price.overrideEnabled else { return }
        model.perform { try await $0.setOverride(o, enabled: on) }
    }

    func refreshSlug() async {
        let snap = await model.store.lastSnapshotAt
        let reload = await model.store.lastWidgetReloadAt
        slug = Slug(snapshot: snap ?? model.store.readSnapshot()?.generatedAt, reload: reload)
    }

    func makeExport() async {
        guard let data = try? await model.store.exportJSON() else { return }
        let url = FileManager.default.temporaryDirectory.appendingPathComponent("verso-export.json")
        do { try data.write(to: url, options: .atomic); exportURL = url } catch {}
    }
}

/// The thirteen schedule rows: year / month / fraction, prefilled with the standard shape.
struct ScheduleEditor: View {
    @Binding var schedule: [Tranche]
    @State private var open = false
    var body: some View {
        DisclosureGroup(isExpanded: $open) {
            VStack(spacing: 2) {
                ForEach(schedule.indices, id: \.self) { i in
                    HStack(spacing: 8) {
                        Text(Roman.chapter(i) + ".").font(VersoFont.serif(.subheadline)).frame(width: 44, alignment: .leading)
                        TextField("Year", value: $schedule[i].year, format: .number.grouping(.never)).keyboardType(.numberPad).frame(width: 52)
                        Text("/").foregroundStyle(Palette.fadedInk)
                        TextField("Mo", value: $schedule[i].month, format: .number).keyboardType(.numberPad).frame(width: 28)
                        Spacer()
                        TextField("0.0625", value: $schedule[i].fraction, format: .percent.precision(.fractionLength(0...3)))
                            .keyboardType(.decimalPad).multilineTextAlignment(.trailing).frame(width: 72)
                    }
                    .font(VersoFont.ledger(.subheadline))
                    .foregroundStyle(Palette.ink)
                }
                let sum = schedule.reduce(0) { $0 + $1.fraction }
                HStack {
                    Text("Σ \(sum.formatted(.percent.precision(.fractionLength(0...2))))")
                        .font(VersoFont.ledger(.caption)).foregroundStyle(abs(sum - 1) < 0.0001 ? Palette.fadedInk : Palette.rust)
                    Spacer()
                    QuietButton("Reset to standard") { schedule = Tranche.standard }
                }
                .padding(.top, 6)
            }
            .padding(.top, 8)
        } label: {
            Text("The schedule · \(schedule.count) chapters").font(VersoFont.body(16)).foregroundStyle(Palette.ink)
        }
        .tint(Palette.fadedInk)
    }
}
