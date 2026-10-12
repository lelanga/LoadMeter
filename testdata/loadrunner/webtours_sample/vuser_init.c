/* Hand-written fixture: session setup for the Load Meter mock shop. */
vuser_init()
{
	web_set_max_html_param_len("2048");
	web_set_timeout("STEP", "60");
	web_cleanup_cookies();
	web_add_auto_header("X-Client", "loadmeter-sample");
	web_set_sockets_option("SSL_VERSION", "TLS1.2");
	lr_output_message("vuser %s starting", lr_eval_string("{VuNum}"));
	return 0;
}
