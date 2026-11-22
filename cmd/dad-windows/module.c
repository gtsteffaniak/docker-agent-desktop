/*
 * dad-windows: a tiny GTK 3 module loaded into every app under Broadway.
 *
 * It keeps one keyboard fix. GTK treats plain printable keys as menubar mnemonics: pressing h
 * opens Help and the keys that follow are swallowed. Under this desktop the apps are iframes
 * in the web desktop, so typing must reach the focused widget. Printable keys typed without
 * Ctrl, Alt or Super go straight to the focused widget. Shortcuts with modifiers, function
 * keys and everything else keep the normal GTK path.
 *
 * It also strips each app window's decorations and maximizes it, so the app fills the frame the
 * web desktop gives it. Dragging, resizing, minimize, maximize and corners belong to the web
 * desktop (cmd/dad/desktop/).
 *
 * Load it with GTK_MODULES=dad-windows and GTK_PATH pointing at its install prefix.
 */
#include <gtk/gtk.h>
#include <string.h>

static gboolean on_key_press(GtkWidget *widget, GdkEventKey *ev, gpointer data) {
  (void)data;
  GtkWidget *focus = gtk_window_get_focus(GTK_WINDOW(widget));
  if (g_getenv("DAD_DEBUG_KEYS")) {
    g_printerr("dad-debug key-press keyval=0x%x hw=%u state=0x%x focus=%s\n", ev->keyval,
               ev->hardware_keycode, ev->state, focus ? G_OBJECT_TYPE_NAME(focus) : "none");
  }
  const GdkModifierType shortcut = GDK_CONTROL_MASK | GDK_MOD1_MASK | GDK_SUPER_MASK;
  if (focus && !(ev->state & shortcut) && gdk_keyval_to_unicode(ev->keyval) != 0) {
    if (gtk_widget_event(focus, (GdkEvent *)ev)) return TRUE;
  }
  return FALSE;
}

static gboolean is_plain_toplevel(GtkWindow *w) {
  return gtk_window_get_window_type(w) == GTK_WINDOW_TOPLEVEL;
}

/* Connect once per window, before GTK's own handler, so the fix runs first. */
static gboolean on_show(GSignalInvocationHint *hint, guint n, const GValue *params, gpointer data) {
  (void)hint;
  (void)data;
  if (n < 1) return TRUE;
  gpointer obj = g_value_get_object(&params[0]);
  if (!obj || !GTK_IS_WINDOW(obj)) return TRUE;
  GtkWindow *w = GTK_WINDOW(obj);
  if (!is_plain_toplevel(w)) return TRUE;
  if (g_object_get_data(G_OBJECT(w), "dad-windows-keys")) return TRUE;
  g_object_set_data(G_OBJECT(w), "dad-windows-keys", GINT_TO_POINTER(1));
  g_signal_connect(w, "key-press-event", G_CALLBACK(on_key_press), NULL);
  /* The web desktop draws the frame, so the app's own window is bare and fills its display.
   * Undecorated, then maximized: under Broadway that is the whole display. */
  gtk_window_set_decorated(w, FALSE);
  gtk_window_maximize(w);
  return TRUE;
}

G_MODULE_EXPORT void gtk_module_init(gint *argc, gchar ***argv) {
  (void)argc;
  (void)argv;
  g_type_class_ref(GTK_TYPE_WIDGET);
  guint show = g_signal_lookup("show", GTK_TYPE_WIDGET);
  if (show) g_signal_add_emission_hook(show, 0, on_show, NULL, NULL);
}
