package com.loadmeter;

import java.awt.BorderLayout;
import java.awt.Desktop;
import java.awt.Dimension;
import java.awt.FlowLayout;
import java.awt.Font;
import java.io.BufferedReader;
import java.io.File;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.StandardCopyOption;
import java.util.ArrayList;
import java.util.List;
import java.util.regex.Matcher;
import java.util.regex.Pattern;
import javax.swing.BorderFactory;
import javax.swing.JButton;
import javax.swing.JCheckBox;
import javax.swing.JFileChooser;
import javax.swing.JFrame;
import javax.swing.JLabel;
import javax.swing.JOptionPane;
import javax.swing.JPanel;
import javax.swing.JScrollPane;
import javax.swing.JTextArea;
import javax.swing.JTextField;
import javax.swing.SwingUtilities;
import javax.swing.UIManager;
import javax.swing.WindowConstants;

/**
 * Load Meter desktop launcher. One jar, no install: it unpacks the bundled
 * Load Meter engine for this operating system, starts it, and opens the UI in
 * an app window. Closing this window stops the engine.
 */
public final class Launcher {
    private static final Pattern URL_LINE = Pattern.compile("Load Meter UI: (http://127\\.0\\.0\\.1:\\d+/\\S*)");

    private final JFrame frame = new JFrame("Load Meter");
    private final JLabel status = new JLabel("Starting...");
    private final JTextArea log = new JTextArea();
    private final JTextField workspace = new JTextField();
    private final JCheckBox useBrowser = new JCheckBox("Open in my default browser instead of an app window");
    private final JButton start = new JButton("Start");
    private final JButton openBrowser = new JButton("Open in browser");
    private volatile Process engine;
    private volatile String url;

    public static void main(String[] args) {
        try {
            UIManager.setLookAndFeel(UIManager.getSystemLookAndFeelClassName());
        } catch (Exception ignored) {
            // the default look and feel is fine
        }
        SwingUtilities.invokeLater(() -> new Launcher().show());
    }

    private void show() {
        workspace.setText(new File(System.getProperty("user.home"), "LoadMeter-Workspace").getPath());
        log.setEditable(false);
        log.setFont(new Font(Font.MONOSPACED, Font.PLAIN, 12));
        status.setFont(status.getFont().deriveFont(Font.BOLD, 15f));

        JButton browse = new JButton("Change...");
        browse.addActionListener(e -> {
            JFileChooser fc = new JFileChooser(workspace.getText());
            fc.setFileSelectionMode(JFileChooser.DIRECTORIES_ONLY);
            if (fc.showOpenDialog(frame) == JFileChooser.APPROVE_OPTION) {
                workspace.setText(fc.getSelectedFile().getPath());
            }
        });
        start.addActionListener(e -> restart());
        openBrowser.addActionListener(e -> openInBrowser());
        openBrowser.setEnabled(false);
        JButton quit = new JButton("Quit");
        quit.addActionListener(e -> quit());

        JPanel top = new JPanel(new BorderLayout(8, 8));
        top.setBorder(BorderFactory.createEmptyBorder(10, 10, 4, 10));
        top.add(status, BorderLayout.NORTH);
        JPanel ws = new JPanel(new BorderLayout(6, 0));
        ws.add(new JLabel("Workspace:"), BorderLayout.WEST);
        ws.add(workspace, BorderLayout.CENTER);
        ws.add(browse, BorderLayout.EAST);
        JPanel mid = new JPanel(new BorderLayout(0, 4));
        mid.add(ws, BorderLayout.NORTH);
        mid.add(useBrowser, BorderLayout.SOUTH);
        top.add(mid, BorderLayout.CENTER);

        JPanel buttons = new JPanel(new FlowLayout(FlowLayout.LEFT));
        buttons.add(start);
        buttons.add(openBrowser);
        buttons.add(quit);

        JPanel north = new JPanel(new BorderLayout());
        north.add(top, BorderLayout.CENTER);
        north.add(buttons, BorderLayout.SOUTH);

        frame.setLayout(new BorderLayout());
        frame.add(north, BorderLayout.NORTH);
        JScrollPane sp = new JScrollPane(log);
        sp.setBorder(BorderFactory.createTitledBorder("Engine log"));
        frame.add(sp, BorderLayout.CENTER);
        frame.setPreferredSize(new Dimension(720, 440));
        frame.setDefaultCloseOperation(WindowConstants.DO_NOTHING_ON_CLOSE);
        frame.addWindowListener(new java.awt.event.WindowAdapter() {
            @Override
            public void windowClosing(java.awt.event.WindowEvent e) {
                quit();
            }
        });
        Runtime.getRuntime().addShutdownHook(new Thread(this::stopEngine));
        frame.pack();
        frame.setLocationRelativeTo(null);
        frame.setVisible(true);
        restart();
    }

    private void restart() {
        stopEngine();
        start.setEnabled(false);
        openBrowser.setEnabled(false);
        status.setText("Starting Load Meter...");
        final String ws = workspace.getText().trim();
        final boolean browserMode = useBrowser.isSelected();
        Thread t = new Thread(() -> run(ws, browserMode), "engine");
        t.setDaemon(true);
        t.start();
    }

    private void run(String ws, boolean browserMode) {
        try {
            File exe = extractEngine();
            List<String> cmd = new ArrayList<>();
            cmd.add(exe.getPath());
            cmd.add("serve");
            cmd.add("--addr");
            cmd.add("127.0.0.1:0");
            cmd.add("--workspace");
            cmd.add(ws);
            cmd.add(browserMode ? "--open" : "--app");
            ProcessBuilder pb = new ProcessBuilder(cmd);
            pb.redirectErrorStream(true);
            Process p = pb.start();
            engine = p;
            try (BufferedReader r = new BufferedReader(new InputStreamReader(p.getInputStream(), StandardCharsets.UTF_8))) {
                String line;
                while ((line = r.readLine()) != null) {
                    final String l = line;
                    Matcher m = URL_LINE.matcher(l);
                    if (m.find()) {
                        url = m.group(1);
                        SwingUtilities.invokeLater(() -> {
                            status.setText("Running. Load Meter is open in its own window.");
                            openBrowser.setEnabled(true);
                        });
                        // never show the access token in the log
                        append("Load Meter UI is up at " + url.replaceAll("token=\\S+", "token=...") + "\n");
                    } else {
                        append(l + "\n");
                    }
                }
            }
            int code = p.waitFor();
            SwingUtilities.invokeLater(() -> {
                status.setText(code == 0 || code == 1 && engine == null
                        ? "Stopped. Click Start to open Load Meter again."
                        : "Stopped (exit code " + code + "). See the log, then click Start.");
                start.setText("Start");
                start.setEnabled(true);
                openBrowser.setEnabled(false);
            });
        } catch (Exception e) {
            append("ERROR: " + e + "\n");
            SwingUtilities.invokeLater(() -> {
                status.setText("Could not start: " + e.getMessage());
                start.setEnabled(true);
            });
        }
    }

    /** Unpacks the engine for this OS/CPU from the jar (once per build). */
    private File extractEngine() throws IOException {
        String os = System.getProperty("os.name", "").toLowerCase();
        String arch = System.getProperty("os.arch", "").toLowerCase();
        String osName = os.contains("win") ? "windows" : os.contains("mac") ? "darwin" : "linux";
        String archName = arch.contains("aarch64") || arch.contains("arm64") ? "arm64" : "amd64";
        String name = "loadmeter" + (osName.equals("windows") ? ".exe" : "");
        String res = "/natives/" + osName + "-" + archName + "/" + name;
        String build = readResource("/natives/BUILD").trim();
        File dir = new File(System.getProperty("user.home"), ".loadmeter" + File.separator + "jar-engine" + File.separator + build + File.separator + osName + "-" + archName);
        File exe = new File(dir, name);
        try (InputStream in = Launcher.class.getResourceAsStream(res)) {
            if (in == null) {
                throw new IOException("This jar has no engine for " + osName + "/" + archName);
            }
            if (exe.isFile()) {
                return exe;
            }
            if (!dir.isDirectory() && !dir.mkdirs()) {
                throw new IOException("Cannot create " + dir);
            }
            File tmp = new File(dir, name + ".part");
            Files.copy(in, tmp.toPath(), StandardCopyOption.REPLACE_EXISTING);
            if (!tmp.setExecutable(true)) {
                append("warning: could not mark the engine executable\n");
            }
            Files.move(tmp.toPath(), exe.toPath(), StandardCopyOption.REPLACE_EXISTING);
        }
        append("Unpacked engine to " + exe + "\n");
        return exe;
    }

    private static String readResource(String res) throws IOException {
        try (InputStream in = Launcher.class.getResourceAsStream(res)) {
            if (in == null) {
                return "dev";
            }
            java.io.ByteArrayOutputStream out = new java.io.ByteArrayOutputStream();
            byte[] buf = new byte[256];
            int n;
            while ((n = in.read(buf)) > 0) {
                out.write(buf, 0, n);
            }
            return new String(out.toByteArray(), StandardCharsets.UTF_8);
        }
    }

    private void openInBrowser() {
        String u = url;
        if (u == null) {
            return;
        }
        try {
            if (Desktop.isDesktopSupported() && Desktop.getDesktop().isSupported(Desktop.Action.BROWSE)) {
                Desktop.getDesktop().browse(URI.create(u));
            } else {
                JOptionPane.showMessageDialog(frame, "Open this address in your browser:\n" + u.replaceAll("\\?token=.*", "") + "\n(use the engine log's full link)");
            }
        } catch (Exception e) {
            append("Could not open the browser: " + e + "\n");
        }
    }

    private void append(String s) {
        SwingUtilities.invokeLater(() -> {
            log.append(s);
            log.setCaretPosition(log.getDocument().getLength());
        });
    }

    private void stopEngine() {
        Process p = engine;
        engine = null;
        if (p != null && p.isAlive()) {
            p.destroy();
            try {
                if (!p.waitFor(3, java.util.concurrent.TimeUnit.SECONDS)) {
                    p.destroyForcibly();
                }
            } catch (InterruptedException e) {
                p.destroyForcibly();
                Thread.currentThread().interrupt();
            }
        }
    }

    private void quit() {
        stopEngine();
        frame.dispose();
        System.exit(0);
    }
}
