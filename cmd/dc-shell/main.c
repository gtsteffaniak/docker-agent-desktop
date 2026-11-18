/*
 * dc-shell — minimal GTK 3 dock for Phase 0 (Broadway display).
 * Reserves a bottom strip (DOCK_HEIGHT) and exposes buttons to launch apps.
 */
#include <gtk/gtk.h>
#include <stdlib.h>
#include <string.h>

#define DOCK_HEIGHT 48

static int desktop_h = 800;
static GtkWidget *status;

static void launch(const char *argv0) {
  pid_t pid = fork();
  if (pid == 0) {
    execlp(argv0, argv0, NULL);
    _exit(127);
  }
}

static void on_terminal(GtkWidget *w, gpointer d) { (void)w; (void)d; launch("lxterminal"); }
static void on_editor(GtkWidget *w, gpointer d) { (void)w; (void)d; launch("mousepad"); }
static void on_files(GtkWidget *w, gpointer d) { (void)w; (void)d; launch("thunar"); }

static gboolean position_dock(GtkWidget *win) {
  gint w = gdk_screen_width();
  gint h = gdk_screen_height();
  desktop_h = h;
  gtk_window_resize(GTK_WINDOW(win), w, DOCK_HEIGHT);
  gtk_window_move(GTK_WINDOW(win), 0, h - DOCK_HEIGHT);
  gtk_window_set_keep_above(GTK_WINDOW(win), TRUE);
  char buf[128];
  snprintf(buf, sizeof(buf), "dock strip y=%d height=%d (reserved)", h - DOCK_HEIGHT, DOCK_HEIGHT);
  gtk_label_set_text(GTK_LABEL(status), buf);
  return FALSE;
}

static void on_screen_changed(GdkScreen *screen, gpointer data) {
  (void)screen;
  position_dock(GTK_WIDGET(data));
}

int main(int argc, char **argv) {
  (void)argv;
  gtk_init(&argc, &argv);

  GtkWidget *win = gtk_window_new(GTK_WINDOW_TOPLEVEL);
  gtk_window_set_decorated(GTK_WINDOW(win), FALSE);
  gtk_window_set_resizable(GTK_WINDOW(win), FALSE);
  gtk_widget_set_name(win, "dc-shell-dock");

  GtkWidget *box = gtk_box_new(GTK_ORIENTATION_HORIZONTAL, 8);
  gtk_container_add(GTK_CONTAINER(win), box);

  GtkWidget *b1 = gtk_button_new_with_label("Terminal");
  GtkWidget *b2 = gtk_button_new_with_label("Editor");
  GtkWidget *b3 = gtk_button_new_with_label("Files");
  g_signal_connect(b1, "clicked", G_CALLBACK(on_terminal), NULL);
  g_signal_connect(b2, "clicked", G_CALLBACK(on_editor), NULL);
  g_signal_connect(b3, "clicked", G_CALLBACK(on_files), NULL);
  gtk_box_pack_start(GTK_BOX(box), b1, FALSE, FALSE, 0);
  gtk_box_pack_start(GTK_BOX(box), b2, FALSE, FALSE, 0);
  gtk_box_pack_start(GTK_BOX(box), b3, FALSE, FALSE, 0);

  status = gtk_label_new("dc-shell");
  gtk_box_pack_end(GTK_BOX(box), status, TRUE, TRUE, 0);

  g_signal_connect(win, "destroy", G_CALLBACK(gtk_main_quit), NULL);
  gtk_widget_show_all(win);
  g_timeout_add(200, (GSourceFunc)position_dock, win);
  GdkScreen *screen = gtk_widget_get_screen(win);
  g_signal_connect(screen, "size-changed", G_CALLBACK(on_screen_changed), win);

  gtk_main();
  return 0;
}