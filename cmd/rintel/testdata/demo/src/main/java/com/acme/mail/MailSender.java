package com.acme.mail;

public interface MailSender {
    void send(String to, String body);
}
