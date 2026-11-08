package com.wanderstamp.tv

import android.annotation.SuppressLint
import android.app.Activity
import android.graphics.Color
import android.graphics.Typeface
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.view.Gravity
import android.view.KeyEvent
import android.view.View
import android.view.WindowManager
import android.webkit.CookieManager
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.FrameLayout
import android.widget.LinearLayout
import android.widget.TextView

/**
 * One full-screen WebView on the door's /tv page (BuildConfig.TV_URL).
 * The page does everything; this app only keeps the screen on, keeps the
 * pairing cookie, hands Back to the page first, and says when the home
 * server can't be reached.
 */
class MainActivity : Activity() {
	private lateinit var web: WebView
	private lateinit var offline: View
	private val main = Handler(Looper.getMainLooper())
	private var failed = false
	private val retry = Runnable { reload() }

	@SuppressLint("SetJavaScriptEnabled")
	override fun onCreate(savedInstanceState: Bundle?) {
		super.onCreate(savedInstanceState)
		window.addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)

		web = WebView(this).apply {
			setBackgroundColor(NAVY)
			settings.javaScriptEnabled = true
			settings.domStorageEnabled = true
			settings.mediaPlaybackRequiresUserGesture = false
			settings.userAgentString = settings.userAgentString + " WanderstampTV/" + BuildConfig.VERSION_NAME
			isFocusable = true
			isFocusableInTouchMode = true
			webViewClient = Client()
		}
		CookieManager.getInstance().setAcceptCookie(true)

		offline = offlineView()
		offline.visibility = View.GONE

		val root = FrameLayout(this)
		root.setBackgroundColor(NAVY)
		root.addView(web, FrameLayout.LayoutParams(-1, -1))
		root.addView(offline, FrameLayout.LayoutParams(-1, -1))
		setContentView(root)

		if (savedInstanceState == null || !restore(savedInstanceState)) web.loadUrl(BuildConfig.TV_URL)
		web.requestFocus()
	}

	private fun restore(state: Bundle): Boolean = web.restoreState(state) != null

	override fun onSaveInstanceState(outState: Bundle) {
		super.onSaveInstanceState(outState)
		web.saveState(outState)
	}

	override fun onResume() {
		super.onResume()
		web.onResume()
		if (failed) reload()
	}

	override fun onPause() {
		// the pairing cookie must reach the disk before the TV sleeps
		CookieManager.getInstance().flush()
		web.onPause()
		main.removeCallbacks(retry)
		super.onPause()
	}

	override fun onDestroy() {
		main.removeCallbacks(retry)
		web.destroy()
		super.onDestroy()
	}

	/**
	 * Back goes to the page first (WSTV.back() returns true when it used it);
	 * only when the page is on the shelf or the pairing code does the app close.
	 * Handled here rather than in onBackPressed, which newer Android skips.
	 */
	override fun dispatchKeyEvent(event: KeyEvent): Boolean {
		val back = event.keyCode == KeyEvent.KEYCODE_BACK || event.keyCode == KeyEvent.KEYCODE_ESCAPE
		if (!back) {
			if (failed && event.action == KeyEvent.ACTION_UP &&
				(event.keyCode == KeyEvent.KEYCODE_DPAD_CENTER || event.keyCode == KeyEvent.KEYCODE_ENTER)) {
				reload()
				return true
			}
			return super.dispatchKeyEvent(event)
		}
		if (event.action != KeyEvent.ACTION_UP) return true
		if (failed) {
			finish()
			return true
		}
		web.evaluateJavascript("(function(){try{return !!(window.WSTV&&WSTV.back())}catch(e){return false}})()") { used ->
			if (used != "true") finish()
		}
		return true
	}

	private fun reload() {
		main.removeCallbacks(retry)
		failed = false
		offline.visibility = View.GONE
		web.loadUrl(BuildConfig.TV_URL)
		web.requestFocus()
	}

	private fun showOffline() {
		failed = true
		offline.visibility = View.VISIBLE
		main.removeCallbacks(retry)
		main.postDelayed(retry, RETRY_MS)
	}

	private inner class Client : WebViewClient() {
		// stays on the door: any other link is ignored, never opened
		override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
			val u = request.url
			val home = android.net.Uri.parse(BuildConfig.TV_URL)
			return !(u.host == home.host && u.port == home.port)
		}

		override fun onReceivedError(view: WebView, request: WebResourceRequest, error: WebResourceError) {
			if (request.isForMainFrame) showOffline()
		}
	}

	private fun offlineView(): View {
		val box = LinearLayout(this)
		box.orientation = LinearLayout.VERTICAL
		box.gravity = Gravity.CENTER
		box.setBackgroundColor(NAVY)
		val pad = (48 * resources.displayMetrics.density).toInt()
		box.setPadding(pad, pad, pad, pad)
		val title = TextView(this)
		title.setText(R.string.offline_title)
		title.setTextColor(PAPER)
		title.textSize = 34f
		title.typeface = Typeface.SERIF
		title.gravity = Gravity.CENTER
		val body = TextView(this)
		body.setText(R.string.offline_body)
		body.setTextColor(PAPER)
		body.textSize = 20f
		body.typeface = Typeface.SERIF
		body.gravity = Gravity.CENTER
		body.setPadding(0, pad / 2, 0, 0)
		box.addView(title)
		box.addView(body)
		return box
	}

	private companion object {
		const val RETRY_MS = 15_000L
		val NAVY = Color.rgb(0x14, 0x26, 0x39)
		val PAPER = Color.rgb(0xf6, 0xf1, 0xe3)
	}
}
