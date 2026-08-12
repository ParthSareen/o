import SwiftUI

struct ChatView: View {
    let controller: SessionController

    var body: some View {
        VStack(spacing: 0) {
            if let banner = controller.errorBanner {
                HStack(spacing: 8) {
                    Image(systemName: "exclamationmark.triangle.fill")
                    Text(banner)
                        .lineLimit(3)
                        .font(.callout)
                    Spacer()
                }
                .foregroundStyle(.red)
                .padding(10)
                .background(Color.red.opacity(0.08))
            }

            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 12) {
                        ForEach(controller.blocks) { block in
                            BlockRow(block: block)
                                .id(block.id)
                        }
                        if !controller.liveThinking.isEmpty && controller.liveAssistant.isEmpty {
                            LiveRow(icon: "brain", text: controller.liveThinking, dimmed: true)
                                .id("live-thinking")
                        }
                        if !controller.liveAssistant.isEmpty {
                            LiveRow(icon: "sparkle", text: controller.liveAssistant, dimmed: false)
                                .id("live-assistant")
                        } else if controller.phase == .running {
                            HStack(spacing: 8) {
                                ProgressView().controlSize(.small)
                                Text("o is working…")
                                    .font(.caption)
                                    .foregroundStyle(.secondary)
                            }
                            .padding(.leading, 26)
                            .id("working")
                        }
                        Color.clear.frame(height: 1).id("bottom")
                    }
                    .padding(.horizontal, 12)
                    .padding(.vertical, 10)
                }
                .onChange(of: controller.blocks.count) { _, _ in
                    proxy.scrollTo("bottom", anchor: .bottom)
                }
                .onChange(of: controller.liveAssistant) { _, _ in
                    proxy.scrollTo("bottom", anchor: .bottom)
                }
            }

            Divider()
            ComposerView(controller: controller)
        }
    }
}

/// Streaming text: plain Text (no markdown work per frame) until the run
/// finishes, when it becomes a full AssistantRow.
struct LiveRow: View {
    let icon: String
    let text: String
    let dimmed: Bool

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: icon)
                .foregroundStyle(.secondary)
                .frame(width: 18)
            Text(text)
                .foregroundStyle(dimmed ? .secondary : .primary)
                .italic(dimmed)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}
